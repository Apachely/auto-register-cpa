import { describe, expect, test } from 'bun:test';
import {
  findImportDestinations,
  MAX_KEY_IMPORT_ROWS,
  runProviderKeyImport,
  type ImportUpdate,
} from '@/features/providerKeyImport/batch';

const etag = `"${'a'.repeat(64)}"`;
const destination = { name: 'FreeAi', baseUrl: 'https://freeai.example.invalid/v1', prefix: '' };
const oldKey = { 'api-key': 'test-existing-key', 'proxy-url': 'http://127.0.0.1:12345', weight: 3 };
const provider = {
  name: destination.name,
  'base-url': destination.baseUrl,
  models: [{ name: 'demo-model', alias: 'demo', 'display-name': 'Demo' }],
  headers: { 'X-Test': 'keep' },
  priority: 2,
  keys: [oldKey],
};
const other = {
  name: 'Separate provider',
  'base-url': 'https://other.example.invalid/v1',
  keys: [{ 'api-key': 'test-other-key', weight: 5 }],
};

function fixture(overrides?: Partial<Parameters<typeof runProviderKeyImport>[0]>) {
  const writes: { groups: Record<string, unknown>[]; etag: string }[] = [];
  const updates: ImportUpdate[] = [];
  const client: Parameters<typeof runProviderKeyImport>[0] = {
    read: async () => ({ groups: [provider, other], etag }),
    write: async (groups, version) => {
      writes.push({ groups, etag: version });
    },
    isCurrent: () => true,
    ...overrides,
  };
  return {
    writes,
    updates,
    run: (source: string) =>
      runProviderKeyImport(client, destination, source, (update) => updates.push(update)),
  };
}

const read =
  (groups: Record<string, unknown>[], version = etag) =>
  async () => ({ groups, etag: version });

function deferred() {
  let release!: () => void;
  const promise = new Promise<void>((resolve) => {
    release = resolve;
  });
  return { promise, release };
}

describe('provider key import', () => {
  test('appends only valid new entries to the selected provider and keeps its existing configuration', async () => {
    const { run, updates, writes } = fixture();
    await run(
      '\r\ntest-existing-key\r\nuser@example.invalid|test-password\r\n test-new-key \r\ntest-new-key\r\n'
    );
    expect(writes).toEqual([
      {
        etag,
        groups: [
          {
            ...provider,
            keys: [oldKey, { 'api-key': 'test-new-key', 'proxy-url': oldKey['proxy-url'] }],
          },
          other,
        ],
      },
    ]);
    expect(provider.keys).toEqual([oldKey]);
    expect(updates.map(({ stage }) => stage)).toEqual([
      'validating',
      'reading',
      'writing',
      'finished',
    ]);
    expect(updates.at(-1)).toEqual({
      stage: 'finished',
      rows: [
        { line: 2, status: 'already_present' },
        { line: 3, status: 'invalid' },
        { line: 4, status: 'added' },
        { line: 5, status: 'duplicate' },
      ],
    });
    expect(JSON.stringify(updates)).not.toMatch(/test-new-key|test-password|test-existing-key/);
  });

  test('shows writing progress before CPA acknowledges the save', async () => {
    const entered = deferred();
    const acknowledge = deferred();
    const { run, updates } = fixture({
      write: async () => {
        entered.release();
        await acknowledge.promise;
      },
    });
    const operation = run('test-new-key');
    await entered.promise;
    expect(updates.at(-1)).toMatchObject({
      stage: 'writing',
      rows: [{ line: 1, status: 'pending' }],
    });
    acknowledge.release();
    await operation;
    expect(updates.at(-1)).toMatchObject({
      stage: 'finished',
      rows: [{ line: 1, status: 'added' }],
    });
  });

  test('leaves configured routing alone for a provider without existing key-level overrides', async () => {
    const { run, writes } = fixture({ read: read([{ ...provider, keys: [] }]) });
    await run('test-new-key');
    expect(writes[0]?.groups[0]?.keys).toEqual([{ 'api-key': 'test-new-key' }]);
  });

  test('does not write when all rows were duplicates or invalid', async () => {
    const { run, updates, writes } = fixture();
    await run('test-existing-key\ntest-existing-key\nAuthorization: Bearer fake');
    expect(writes).toHaveLength(0);
    expect(updates.at(-1)?.rows.map(({ status }) => status)).toEqual([
      'already_present',
      'duplicate',
      'invalid',
    ]);
  });

  test('bounds a batch before calling CPA', async () => {
    const { run, updates } = fixture({
      read: async () => {
        throw new Error('Unexpected read');
      },
    });
    await run(
      Array.from({ length: MAX_KEY_IMPORT_ROWS + 1 }, (_, i) => `test-key-${i}`).join('\n')
    );
    expect(updates.map(({ stage }) => stage)).toEqual(['validating', 'finished']);
    expect(updates.at(-1)?.error).toBe('batch_limit');
  });

  test('refuses missing ETags and stale or ambiguous targets', async () => {
    const cases = [
      { read: read([provider], ''), error: 'config_unavailable' },
      { read: read([provider], `W/${etag}`), error: 'config_unavailable' },
      { read: read([other]), error: 'destination_changed' },
      { read: read([provider, { ...provider }]), error: 'destination_changed' },
      {
        read: read([
          provider,
          { ...provider, 'base-url': 'https://FREEAI.example.invalid:443/v1' },
        ]),
        error: 'destination_changed',
      },
    ];
    for (const item of cases) {
      const { run, updates, writes } = fixture({ read: item.read });
      await run('test-new-key');
      expect(writes).toHaveLength(0);
      expect(updates.at(-1)).toMatchObject({
        error: item.error,
        rows: [{ status: 'not_written' }],
      });
    }
  });

  test('does not reserialize another provider when its number cannot round-trip safely', async () => {
    const { run, updates, writes } = fixture({
      read: read([provider, { ...other, priority: Number.MAX_SAFE_INTEGER + 1 }]),
    });
    await run('test-new-key');
    expect(writes).toHaveLength(0);
    expect(updates.at(-1)?.error).toBe('config_unavailable');
  });

  test('does not choose between mixed existing egress settings', async () => {
    const { run, updates, writes } = fixture({
      read: read([{ ...provider, keys: [oldKey, { 'api-key': 'test-second-key' }] }]),
    });
    await run('test-new-key');
    expect(writes).toHaveLength(0);
    expect(updates.at(-1)?.error).toBe('proxy_mismatch');
  });

  test('does not write or publish stale results after changing CPA connections', async () => {
    const pending = deferred();
    let current = true;
    const { run, updates, writes } = fixture({
      isCurrent: () => current,
      read: async () => {
        await pending.promise;
        return { groups: [provider], etag };
      },
    });
    const operation = run('test-new-key');
    current = false;
    pending.release();
    await operation;
    expect(writes).toHaveLength(0);
    expect(updates.map(({ stage }) => stage)).toEqual(['validating', 'reading']);
  });

  test('distinguishes a conflict from rejected and unconfirmed writes, without retry or detail leakage', async () => {
    const cases = [
      { status: 412, error: 'conflict', row: 'not_written' },
      { status: 422, error: 'write_rejected', row: 'not_written' },
      { status: undefined, error: 'write_unconfirmed', row: 'unknown' },
      { status: 503, error: 'write_unconfirmed', row: 'unknown' },
    ];
    for (const item of cases) {
      let attempts = 0;
      const { run, updates } = fixture({
        write: async () => {
          attempts += 1;
          throw { status: item.status, data: 'test-secret-response' };
        },
      });
      await run('test-new-key');
      expect(attempts).toBe(1);
      expect(updates.at(-1)).toMatchObject({ error: item.error, rows: [{ status: item.row }] });
      expect(JSON.stringify(updates)).not.toContain('test-secret-response');
    }
  });

  test('confirms keys by readback after a lost write response, without resubmitting', async () => {
    let groups: Record<string, unknown>[] = [provider, other];
    let reads = 0;
    let writes = 0;
    const { run, updates } = fixture({
      read: async () => {
        reads += 1;
        return { groups, etag };
      },
      write: async (next) => {
        writes += 1;
        groups = next;
        throw new Error('Connection closed before acknowledgement');
      },
    });
    await run('test-existing-key\ntest-new-key');
    expect(writes).toBe(1);
    expect(reads).toBe(2);
    expect(updates.map(({ stage }) => stage)).toEqual([
      'validating',
      'reading',
      'writing',
      'verifying',
      'finished',
    ]);
    expect(updates.at(-1)).toEqual({
      stage: 'finished',
      rows: [
        { line: 1, status: 'already_present' },
        { line: 2, status: 'confirmed_present' },
      ],
    });
    expect(JSON.stringify(updates)).not.toContain('test-new-key');
  });

  test('confirms presence only at the selected destination, not in other providers', async () => {
    let reads = 0;
    const { run, updates } = fixture({
      read: async () => {
        reads += 1;
        return {
          groups:
            reads === 1
              ? [provider, other]
              : [
                  { ...provider, keys: [oldKey, { 'api-key': 'test-present-key' }] },
                  { ...other, keys: [{ 'api-key': 'test-other-only-key' }] },
                ],
          etag,
        };
      },
      write: async () => {
        throw { status: 503 };
      },
    });
    await run('test-present-key\ntest-other-only-key');
    expect(updates.at(-1)).toEqual({
      stage: 'finished',
      rows: [
        { line: 1, status: 'confirmed_present' },
        { line: 2, status: 'unknown' },
      ],
      error: 'write_unconfirmed',
    });
  });

  test('keeps results unknown when readback is unavailable or the destination is ambiguous', async () => {
    const present = { ...provider, keys: [{ 'api-key': 'test-new-key' }] };
    const readbacks = [
      async () => {
        throw new Error('Unavailable');
      },
      read([present, provider]),
      read([present], ''),
    ];
    for (const readback of readbacks) {
      let reads = 0;
      const { run, updates } = fixture({
        read: () => (++reads === 1 ? read([provider])() : readback()),
        write: async () => {
          throw { status: 408 };
        },
      });
      await run('test-new-key');
      expect(reads).toBe(2);
      expect(updates.at(-1)).toMatchObject({
        error: 'write_unconfirmed',
        rows: [{ status: 'unknown' }],
      });
    }
  });

  test('does not publish readback results after the connection changes', async () => {
    const checking = deferred();
    const release = deferred();
    let current = true;
    let reads = 0;
    const { run, updates } = fixture({
      isCurrent: () => current,
      read: async () => {
        if (++reads === 1) return { groups: [provider], etag };
        checking.release();
        await release.promise;
        return { groups: [{ ...provider, keys: [{ 'api-key': 'test-new-key' }] }], etag };
      },
      write: async () => {
        throw { status: 500 };
      },
    });
    const pending = run('test-new-key');
    await checking.promise;
    expect(updates.at(-1)?.stage).toBe('verifying');
    current = false;
    release.release();
    await pending;
    expect(updates.at(-1)?.stage).toBe('verifying');
  });

  test('only lists distinct, valid endpoints and does not display URL credentials', () => {
    const destinations = findImportDestinations([
      provider,
      { ...provider, 'base-url': 'https://user:secret@freeai.example.invalid/v1' },
      { ...provider, 'base-url': 'https://freeai.example.invalid/v1?token=secret' },
      other,
      { ...other },
    ]);
    expect(destinations).toEqual([{ target: destination, endpoint: destination.baseUrl }]);
  });
});
