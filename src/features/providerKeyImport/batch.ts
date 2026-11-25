import { isRecord } from '@/utils/helpers';

export const MAX_KEY_IMPORT_ROWS = 100;
export const MAX_KEY_IMPORT_LENGTH = MAX_KEY_IMPORT_ROWS * 514;

export interface ImportTarget {
  name: string;
  baseUrl: string;
  prefix: string;
}

export interface ImportDestination {
  target: ImportTarget;
  endpoint: string;
}

export type ImportStage = 'validating' | 'reading' | 'writing' | 'verifying' | 'finished';
export type RowStatus =
  | 'invalid'
  | 'duplicate'
  | 'already_present'
  | 'pending'
  | 'added'
  | 'confirmed_present'
  | 'not_written'
  | 'unknown';

export interface ImportRow {
  line: number;
  status: RowStatus;
}

type ImportError =
  | 'empty'
  | 'batch_limit'
  | 'config_unavailable'
  | 'destination_changed'
  | 'proxy_mismatch'
  | 'conflict'
  | 'write_rejected'
  | 'write_unconfirmed';

export interface ImportUpdate {
  stage: ImportStage;
  rows: readonly ImportRow[];
  error?: ImportError;
}

interface ImportClient {
  read(): Promise<{ groups: Record<string, unknown>[]; etag: string }>;
  write(groups: Record<string, unknown>[], etag: string): Promise<void>;
  isCurrent(): boolean;
}

function targetOf(group: Record<string, unknown>): ImportTarget | null {
  const name = typeof group.name === 'string' ? group.name.trim() : '';
  const baseUrl = typeof group['base-url'] === 'string' ? group['base-url'].trim() : '';
  const prefix = typeof group.prefix === 'string' ? group.prefix.trim() : '';
  if (!name || !/^https?:\/\//i.test(baseUrl)) return null;
  try {
    const url = new URL(baseUrl);
    if (
      !['https:', 'http:'].includes(url.protocol) ||
      url.username ||
      url.password ||
      url.search ||
      url.hash
    )
      return null;
  } catch {
    return null;
  }
  return { name, baseUrl, prefix };
}

function sameTarget(a: ImportTarget | null, b: ImportTarget): boolean {
  return a !== null && a.name === b.name && a.baseUrl === b.baseUrl && a.prefix === b.prefix;
}

export function importDestinationLabel({ target, endpoint }: ImportDestination): string {
  return [target.name, endpoint, target.prefix].filter(Boolean).join(' · ');
}

export function findImportDestinations(groups: Record<string, unknown>[]): ImportDestination[] {
  const counts = new Map<string, number>();
  const candidates = groups.flatMap((group) => {
    const target = targetOf(group);
    if (!target) return [];
    const url = new URL(target.baseUrl);
    const candidate = { target, endpoint: `${url.origin}${url.pathname}` };
    const label = importDestinationLabel(candidate);
    counts.set(label, (counts.get(label) ?? 0) + 1);
    return [candidate];
  });
  // The displayed identity must also be unambiguous after URL normalization.
  return candidates.filter((candidate) => counts.get(importDestinationLabel(candidate)) === 1);
}

function parseRows(source: string) {
  const rows: ImportRow[] = [];
  const keys = new Map<number, string>();
  const seen = new Set<string>();
  for (const [index, value] of source.split(/\r\n?|\n/).entries()) {
    const key = value.trim();
    if (!key) continue;
    const line = index + 1;
    // Only one opaque key per line; URLs, account/password pairs and headers are not imports.
    if (!/^[A-Za-z0-9._~+/=-]{8,512}$/.test(key)) {
      rows.push({ line, status: 'invalid' });
    } else if (seen.has(key)) {
      rows.push({ line, status: 'duplicate' });
    } else {
      seen.add(key);
      keys.set(line, key);
      rows.push({ line, status: 'pending' });
    }
  }
  return { rows, keys };
}

function statusOf(error: unknown) {
  return isRecord(error) && typeof error.status === 'number' ? error.status : undefined;
}

// CPA v8 uses the on-disk SHA-256 as a strong quoted ETag.
const isConfigETag = (etag: string) => /^"[a-f0-9]{64}"$/.test(etag);

function getDestinationKeys(groups: Record<string, unknown>[], target: ImportTarget) {
  if (!findImportDestinations(groups).some((item) => sameTarget(item.target, target))) {
    return { error: 'destination_changed' } as const;
  }
  const group = groups.find((item) => sameTarget(targetOf(item), target));
  const keys = group?.keys;
  if (!group || !Array.isArray(keys) || !keys.every(isRecord)) {
    return { error: 'config_unavailable' } as const;
  }
  return { group, keys };
}

function configuredKeySet(entries: Record<string, unknown>[]): Set<string> {
  return new Set(
    entries.flatMap((entry) =>
      typeof entry['api-key'] === 'string' ? [entry['api-key'].trim()] : []
    )
  );
}

function canRoundTripGroups(groups: Record<string, unknown>[]): boolean {
  // The v8 endpoint replaces the family as a whole. Do not reserialize config
  // numbers the browser may already have rounded or parsed as infinity.
  const remaining: unknown[] = [groups];
  while (remaining.length) {
    const value = remaining.pop();
    if (typeof value === 'number') {
      if (!Number.isFinite(value) || Math.abs(value) > Number.MAX_SAFE_INTEGER) return false;
    } else if (Array.isArray(value)) {
      for (const item of value) remaining.push(item);
    } else if (isRecord(value)) {
      for (const item of Object.values(value)) remaining.push(item);
    }
  }
  return true;
}

/** All progress data is safe to display: only stages, line numbers, outcomes and fixed error codes. */
export async function runProviderKeyImport(
  client: ImportClient,
  destination: ImportTarget,
  source: string,
  onUpdate: (update: ImportUpdate) => void
): Promise<void> {
  let rows: ImportRow[] = [];
  const emit = (stage: ImportStage, error?: ImportError) => {
    if (client.isCurrent()) onUpdate({ stage, rows, error });
  };
  const finish = (status: RowStatus, error?: ImportError) => {
    rows = rows.map((row) => (row.status === 'pending' ? { ...row, status } : row));
    emit('finished', error);
  };
  emit('validating');
  if (!client.isCurrent()) return;
  if (source.length > MAX_KEY_IMPORT_LENGTH) return finish('not_written', 'batch_limit');
  const parsed = parseRows(source);
  try {
    rows = parsed.rows;
    if (rows.length > MAX_KEY_IMPORT_ROWS) {
      rows = [];
      return finish('not_written', 'batch_limit');
    }
    if (rows.length === 0) return finish('not_written', 'empty');
    if (parsed.keys.size === 0) return finish('not_written');

    emit('reading');
    let snapshot: Awaited<ReturnType<ImportClient['read']>>;
    try {
      snapshot = await client.read();
    } catch {
      return finish('not_written', 'config_unavailable');
    }
    if (!client.isCurrent()) return;
    const { groups, etag } = snapshot;
    if (!isConfigETag(etag)) return finish('not_written', 'config_unavailable');
    const target = getDestinationKeys(groups, destination);
    if ('error' in target) return finish('not_written', target.error);
    const { group, keys: existing } = target;
    const seen = configuredKeySet(existing);
    rows = rows.map((row) => {
      const key = parsed.keys.get(row.line);
      if (!key || !seen.has(key)) return row;
      parsed.keys.delete(row.line);
      return { ...row, status: 'already_present' };
    });
    if (parsed.keys.size === 0) return finish('not_written');
    if (!canRoundTripGroups(groups)) return finish('not_written', 'config_unavailable');

    // Keep the current provider's ordinary CPA egress policy. Do not pick a route
    // when existing credentials disagree, and do not contact any proxy/upstream.
    const existingProxies = new Set(
      existing.map((entry) => {
        const proxy = entry['proxy-url'];
        return proxy == null ? '' : typeof proxy === 'string' ? proxy.trim() : null;
      })
    );
    if (existingProxies.has(null) || existingProxies.size > 1) {
      return finish('not_written', 'proxy_mismatch');
    }
    const proxy = existingProxies.values().next().value;
    const additions = Array.from(parsed.keys.values(), (key) => ({
      'api-key': key,
      ...(proxy ? { 'proxy-url': proxy } : {}),
    }));
    const next = groups.map((item) =>
      item === group ? { ...group, keys: [...existing, ...additions] } : item
    );
    emit('writing');
    if (!client.isCurrent()) return;
    try {
      await client.write(next, etag);
      finish('added');
    } catch (error: unknown) {
      const status = statusOf(error);
      if (status === 409 || status === 412) return finish('not_written', 'conflict');
      if (status !== undefined && status >= 400 && status < 500 && status !== 408) {
        return finish('not_written', 'write_rejected');
      }
      // A lost response may have followed a committed write. Read once on the same
      // connection, but never resubmit or interpret absence as proof of a rollback.
      if (!client.isCurrent()) return;
      emit('verifying');
      if (!client.isCurrent()) return;
      let readback: Awaited<ReturnType<ImportClient['read']>>;
      try {
        readback = await client.read();
      } catch {
        return finish('unknown', 'write_unconfirmed');
      }
      if (!client.isCurrent()) return;
      const current = getDestinationKeys(readback.groups, destination);
      if ('error' in current || !isConfigETag(readback.etag)) {
        return finish('unknown', 'write_unconfirmed');
      }
      const persisted = configuredKeySet(current.keys);
      rows = rows.map((row) => {
        const key = parsed.keys.get(row.line);
        return key && persisted.has(key) ? { ...row, status: 'confirmed_present' } : row;
      });
      const incomplete = rows.some(({ status }) => status === 'pending');
      finish('unknown', incomplete ? 'write_unconfirmed' : undefined);
    }
  } finally {
    parsed.keys.clear();
  }
}
