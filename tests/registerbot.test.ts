import { afterEach, describe, expect, mock, spyOn, test } from 'bun:test';
import {
  createEventDecoder,
  consumeEventStream,
  waitForReconnect,
} from '@/features/registerbot/stream';
import {
  createRegisterbotClient,
  readRegisterChannel,
  readRegisterEvent,
  readRegisterProxyPool,
  readRegisterRun,
} from '@/services/api/registerbot';
import { apiClient } from '@/services/api/client';

afterEach(() => mock.restore());
const wireEvent = {
  seq: 9,
  at: 1000,
  channel: 'demo',
  stage: 'waiting_human',
  exit_ip: '203.0.113.5',
};

describe('registration event stream', () => {
  test('decodes split CRLF frames, multiline data, comments and ids', () => {
    const frames: unknown[] = [];
    const decode = createEventDecoder((frame) => frames.push(frame));
    decode(': keepalive\r\n\r\nid: 9\r\nevent: registerbot\r\ndata: {"seq":9,\r\n');
    decode('data: "stage":"queued"}\r\n\r');
    expect(frames).toEqual([]);
    decode('\n');
    expect(frames).toEqual([
      { id: '9', event: 'registerbot', data: '{"seq":9,\n"stage":"queued"}' },
    ]);
  });

  test('handles unicode split between network chunks', async () => {
    const bytes = new TextEncoder().encode('data: 账户\n\n');
    const frames: string[] = [];
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(bytes.slice(0, 7));
        controller.enqueue(bytes.slice(7));
        controller.close();
      },
    });
    await consumeEventStream(stream, new AbortController().signal, (frame) =>
      frames.push(frame.data)
    );
    expect(frames).toEqual(['账户']);
  });

  test('aborting releases a blocked stream and discards subsequent events', async () => {
    const controller = new AbortController();
    let cancelCount = 0;
    const frames: unknown[] = [];
    const stream = new ReadableStream<Uint8Array>({
      cancel() {
        cancelCount += 1;
      },
    });
    const reading = consumeEventStream(stream, controller.signal, (frame) => frames.push(frame));
    controller.abort();
    await reading;
    await waitForReconnect(controller.signal, 60_000);
    expect(cancelCount).toBe(1);
    expect(frames).toEqual([]);
  });

  test('rejects oversized unfinished frames', () => {
    const decode = createEventDecoder(() => undefined);
    expect(() => decode('x'.repeat(1024 * 1024 + 1))).toThrow('too large');
  });
});

describe('registration contracts and connection isolation', () => {
  test('rejects a non-object snapshot instead of showing an empty channel list', async () => {
    const captured = apiClient.captureConnection();
    spyOn(captured, 'get').mockResolvedValue('<html>not the registerbot API</html>');
    spyOn(apiClient, 'captureConnection').mockReturnValue(captured);
    const client = createRegisterbotClient(() => true, new AbortController().signal);

    await expect(client.snapshot()).rejects.toMatchObject({
      apiCode: 'invalid_management_response',
    });
  });

  test('provides labels for every fixed backend stage and failure reason', async () => {
    const locale = await Bun.file('src/i18n/locales/en.json').json();
    const stages = [
      'queued',
      'validating',
      'pinning',
      'browser_start',
      'google_login',
      'onboarding',
      'onboarding_entry',
      'onboarding_profile',
      'onboarding_credit',
      'onboarding_tool',
      'onboarding_test',
      'onboarding_discord',
      'onboarding_plans',
      'onboarding_unknown',
      'onboarding_profile_required',
      'onboarding_credit_pending',
      'onboarding_complete',
      'waiting_human',
      'capturing_key',
      'verifying_key',
      'writing_key',
      'pending_write',
      'done',
      'failed',
      'skipped',
      'paused',
      'email',
      'password',
      'choose_account',
      'consent',
      'google',
      'speedbump',
      'key',
      'reveal',
      'dashboard',
      'challenge',
      'blocked',
      'invalid_credentials',
      'manual',
      'loading',
      'gateway_probe',
      'gateway_degraded',
      'browser_degraded',
      'settings_updated',
      'storage_error',
      'network_error',
      'running',
      'cancelled',
      'partial',
      'proxy_pool_refreshed',
      'proxy_pool_updated',
      'proxy_pool_unavailable',
      'proxy_pool_no_nodes',
      'proxy_pool_rotation_exhausted',
      'proxy_pool_quota_exhausted',
      'proxy_pool_state_failed',
      'account_usable',
      'welcome_credit_missing',
    ];
    const reasons = [
      'verification_failed',
      'provider_missing',
      'config_conflict',
      'write_failed',
      'import_unavailable',
      'verify_or_write_failed',
      'restart_resume_required',
      'operator_paused',
      'operator_cancelled',
      'manual_action_required',
      'attempt_failed',
      'proxy_pool_unavailable',
      'proxy_pool_no_nodes',
      'proxy_pool_rotation_exhausted',
      'proxy_pool_quota_exhausted',
      'proxy_pool_state_failed',
      'welcome_credit_missing',
      'target_reached',
      'invalid',
      'duplicate',
    ];
    for (const key of stages) expect(locale.registerbot.stage[key]).toBeString();
    for (const key of reasons) expect(locale.registerbot.reason[key]).toBeString();
  });
  test('normalizes channel, run and event wire fields', () => {
    expect(
      readRegisterChannel({
        id: 'one',
        enabled: true,
        proxy_url: 'http://127.0.0.1:12450',
        concurrency: 2,
        manual_mode: true,
        onboarding_role: 'employee',
        onboarding_source: 'google',
        skip_optional_discord: true,
        nodes: ['a', 2],
      })
    ).toMatchObject({
      id: 'one',
      enabled: true,
      concurrency: 2,
      manualMode: true,
      onboardingRole: 'employee',
      onboardingSource: 'google',
      skipOptionalDiscord: true,
      proxyUrl: 'http://127.0.0.1:12450',
      nodes: ['a'],
    });
    expect(
      readRegisterRun({
        id: 'run',
        created_at: 1000,
        rows: [{ id: 'row', masked_key: 'sk-***', updated_at: 1500 }],
        counters: {
          waiting_human: 1,
          pending_write: 2,
          harvested_keys: 3,
          usable_accounts: 1,
        },
      })
    ).toMatchObject({
      createdAt: 1000,
      rows: [{ maskedKey: 'sk-***', updatedAt: 1500 }],
      counters: { waiting: 1, pendingWrite: 2, harvested: 3, usable: 1 },
    });
    expect(readRegisterEvent(wireEvent)).toMatchObject({ seq: 9, exitIp: '203.0.113.5' });
    expect(readRegisterEvent({ seq: '9', stage: 'queued' })).toBeNull();
    expect(readRegisterEvent({ seq: 9 })).toBeNull();
  });

  test('normalizes proxy pool quota and node views', () => {
    expect(
      readRegisterProxyPool({
        enabled: true,
        mode: 'enforce',
        max_usable_accounts_per_ip: 3,
        total_nodes: 2,
        ready_nodes: 1,
        usable_accounts: 4,
        nodes: [
          {
            group: 'group',
            node: 'node-a',
            exit_ip: '203.0.113.40',
            status: 'ready',
            usable_accounts: 2,
          },
        ],
        ips: [
          {
            exit_ip: '203.0.113.40',
            usable_accounts: 2,
            unusable_accounts: 1,
            remaining: 1,
          },
        ],
      })
    ).toMatchObject({
      enabled: true,
      mode: 'enforce',
      maxUsableAccountsPerIp: 3,
      totalNodes: 2,
      readyNodes: 1,
      usableAccounts: 4,
      nodes: [{ node: 'node-a', exitIp: '203.0.113.40', status: 'ready' }],
      ips: [{ exitIp: '203.0.113.40', usableAccounts: 2, remaining: 1 }],
    });
  });

  test('rejects a mutation after an ABA connection switch', async () => {
    apiClient.setConfig({ apiBase: 'http://localhost:18317', managementKey: 'test-only-one' });
    const client = createRegisterbotClient(() => true, new AbortController().signal);
    apiClient.setConfig({ apiBase: 'http://localhost:18318', managementKey: 'test-only-two' });
    apiClient.setConfig({ apiBase: 'http://localhost:18317', managementKey: 'test-only-one' });
    expect(client.isCurrent()).toBe(false);
    await expect(client.start('demo', 'test@example.invalid|test-password')).rejects.toThrow(
      'Connection changed'
    );
    await expect(client.action('run', 'resume')).rejects.toThrow('Connection changed');
  });

  test('uses an authenticated captured connection for replay without credentials in query', async () => {
    const frames: unknown[] = [];
    const resets: number[] = [];
    let connected = false;
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(
          new TextEncoder().encode(
            `event: reset\ndata: {"cursor":8}\n\nevent: registerbot\ndata: ${JSON.stringify(wireEvent)}\n\n`
          )
        );
        controller.close();
      },
    });
    const captured = apiClient.captureConnection();
    const request = spyOn(captured, 'getRaw').mockResolvedValue({ data: stream } as Awaited<
      ReturnType<typeof apiClient.getRaw>
    >);
    spyOn(apiClient, 'captureConnection').mockReturnValue(captured);
    const client = createRegisterbotClient(() => true, new AbortController().signal);
    await client.events(
      7,
      (event) => frames.push(event),
      (cursor) => resets.push(cursor),
      () => {
        connected = true;
      }
    );
    expect(request.mock.calls[0]).toMatchObject([
      '/registerbot/events',
      {
        adapter: 'fetch',
        responseType: 'stream',
        timeout: 0,
        params: { after: 7 },
        headers: { Accept: 'text/event-stream', 'Last-Event-ID': '7' },
      },
    ]);
    expect(JSON.stringify(request.mock.calls)).not.toContain('managementKey');
    expect(resets).toEqual([8]);
    expect(frames).toHaveLength(1);
    expect(connected).toBe(true);
  });

  test('suppresses frames once the page stops owning the connection', async () => {
    let active = true;
    const captured = apiClient.captureConnection();
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode(`data: ${JSON.stringify(wireEvent)}\n\n`));
        controller.close();
      },
    });
    spyOn(captured, 'getRaw').mockResolvedValue({ data: stream } as Awaited<
      ReturnType<typeof apiClient.getRaw>
    >);
    spyOn(apiClient, 'captureConnection').mockReturnValue(captured);
    const client = createRegisterbotClient(() => active, new AbortController().signal);
    const events: unknown[] = [];
    await client.events(
      0,
      (event) => events.push(event),
      () => undefined,
      () => {
        active = false;
      }
    );
    expect(events).toEqual([]);
  });
});
