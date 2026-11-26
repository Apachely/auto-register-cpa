import { isRecord } from '@/utils/helpers';
import { consumeEventStream } from '@/features/registerbot/stream';
import type {
  RegisterChannel,
  RegisterCounters,
  RegisterEvent,
  RegisterGateway,
  RegisterProxyPool,
  RegisterProxyPoolSettings,
  RegisterRun,
  RegisterSnapshot,
  RunAction,
} from '@/features/registerbot/types';
import { apiClient } from './client';

const PATH = '/registerbot';
const record = (value: unknown): Record<string, unknown> => (isRecord(value) ? value : {});
const text = (value: unknown) => (typeof value === 'string' ? value : '');
const number = (value: unknown) =>
  typeof value === 'number' && Number.isFinite(value) ? value : 0;
const list = (value: unknown): unknown[] => (Array.isArray(value) ? value : []);
const strings = (value: unknown) =>
  list(value).filter((item): item is string => typeof item === 'string');

export function readRegisterChannel(value: unknown): RegisterChannel {
  const item = record(value);
  return {
    id: text(item.id),
    kind: text(item.kind),
    name: text(item.name),
    enabled: item.enabled === true,
    provider: text(item.provider),
    proxyUrl: text(item.proxy_url),
    concurrency: number(item.concurrency),
    group: text(item.group),
    nodes: strings(item.nodes),
    rotateNode: item.rotate_node === true,
    intervalSeconds: number(item.interval_seconds),
    stepDelayMillis: number(item.step_delay_millis),
    targetKeys: number(item.target_keys),
    maxAttempts: number(item.max_attempts),
    startUrl: text(item.start_url),
    manualMode: item.manual_mode === true,
    onboardingRole: text(item.onboarding_role),
    onboardingSource: text(item.onboarding_source),
    skipOptionalDiscord: item.skip_optional_discord === true,
  };
}

function writeChannel(item: RegisterChannel) {
  return {
    id: item.id,
    kind: item.kind,
    name: item.name,
    enabled: item.enabled,
    provider: item.provider,
    proxy_url: item.proxyUrl,
    concurrency: item.concurrency,
    group: item.group,
    nodes: item.nodes,
    rotate_node: item.rotateNode,
    interval_seconds: item.intervalSeconds,
    step_delay_millis: item.stepDelayMillis,
    target_keys: item.targetKeys,
    max_attempts: item.maxAttempts,
    start_url: item.startUrl,
    manual_mode: item.manualMode,
    onboarding_role: item.onboardingRole,
    onboarding_source: item.onboardingSource,
    skip_optional_discord: item.skipOptionalDiscord,
  };
}

function readCounters(value: unknown): RegisterCounters {
  const item = record(value);
  return {
    total: number(item.total),
    queued: number(item.queued),
    running: number(item.running),
    done: number(item.done),
    failed: number(item.failed),
    skipped: number(item.skipped),
    waiting: number(item.waiting_human),
    pendingWrite: number(item.pending_write),
    harvested: number(item.harvested_keys),
    usable: number(item.usable_accounts),
  };
}

export function readRegisterRun(value: unknown): RegisterRun {
  const item = record(value);
  return {
    id: text(item.id),
    channel: text(item.channel),
    status: text(item.status),
    createdAt: number(item.created_at),
    updatedAt: number(item.updated_at),
    counters: readCounters(item.counters),
    rows: list(item.rows).map((value) => {
      const row = record(value);
      return {
        id: text(row.id),
        line: number(row.line),
        account: text(row.account),
        stage: text(row.stage),
        attempt: number(row.attempt),
        maskedKey: text(row.masked_key),
        node: text(row.node),
        exitIp: text(row.exit_ip),
        creditConfirmed: row.credit_confirmed === true,
        usable: row.usable === true,
        reason: text(row.reason),
        updatedAt: number(row.updated_at),
      };
    }),
  };
}

export function readRegisterProxyPool(value: unknown): RegisterProxyPool {
  const item = record(value);
  return {
    enabled: item.enabled === true,
    mode: text(item.mode) === 'enforce' ? 'enforce' : 'disabled',
    maxUsableAccountsPerIp: number(item.max_usable_accounts_per_ip),
    failureCooldownSeconds: number(item.failure_cooldown_seconds),
    totalNodes: number(item.total_nodes),
    readyNodes: number(item.ready_nodes),
    reserved: number(item.reserved),
    exhaustedIps: number(item.exhausted_ips),
    usableAccounts: number(item.usable_accounts),
    lastNode: text(item.last_node),
    lastExitIp: text(item.last_exit_ip),
    updatedAt: number(item.updated_at),
    nodes: list(item.nodes).map((value) => {
      const node = record(value);
      return {
        group: text(node.group),
        node: text(node.node),
        exitIp: text(node.exit_ip),
        status: text(node.status),
        usableAccounts: number(node.usable_accounts),
        reserved: number(node.reserved),
        assignments: number(node.assignments),
        failures: number(node.failures),
        lastUsedAt: number(node.last_used_at),
      };
    }),
    ips: list(item.ips).map((value) => {
      const ip = record(value);
      return {
        exitIp: text(ip.exit_ip),
        usableAccounts: number(ip.usable_accounts),
        unusableAccounts: number(ip.unusable_accounts),
        reserved: number(ip.reserved),
        remaining: number(ip.remaining),
        lastUsedAt: number(ip.last_used_at),
        lastSuccessAt: number(ip.last_success_at),
      };
    }),
  };
}

function writeProxyPool(settings: RegisterProxyPoolSettings) {
  return {
    mode: settings.mode,
    max_usable_accounts_per_ip: settings.maxUsableAccountsPerIp,
    failure_cooldown_seconds: settings.failureCooldownSeconds,
  };
}

export function readRegisterEvent(value: unknown): RegisterEvent | null {
  const item = record(value);
  if (!Number.isSafeInteger(item.seq) || number(item.seq) <= 0 || !text(item.stage)) return null;
  return {
    seq: number(item.seq),
    at: number(item.at),
    channel: text(item.channel),
    run: text(item.run),
    account: text(item.account),
    stage: text(item.stage),
    level: text(item.level),
    message: text(item.message),
    node: text(item.node),
    exitIp: text(item.exit_ip),
    browser: text(item.browser),
  };
}

export function createRegisterbotClient(isActive: () => boolean, signal: AbortSignal) {
  const revision = apiClient.getConnectionRevision();
  const isCurrent = () =>
    !signal.aborted && isActive() && revision === apiClient.getConnectionRevision();
  const client = apiClient.captureConnection(isCurrent);
  const assertCurrent = () => {
    if (!isCurrent()) throw new Error('Connection changed');
  };

  return {
    isCurrent,
    async snapshot(): Promise<RegisterSnapshot> {
      assertCurrent();
      const item = record(await client.get(PATH, { signal }));
      return {
        enabled: item.enabled === true,
        storageError: item.storage_error === true,
        channels: list(item.channels).map(readRegisterChannel),
        runs: list(item.runs).map(readRegisterRun),
        cursor: number(item.cursor),
      };
    },
    async gateway(): Promise<RegisterGateway> {
      assertCurrent();
      const item = record(await client.get(`${PATH}/gateway`, { signal }));
      return {
        mode: text(item.mode),
        proxyUrl: text(item.proxy_url),
        controllerUrl: text(item.controller_url),
        shareMode: text(item.share_mode),
        warnings: strings(item.warnings),
        error: text(item.error),
        groups: list(item.groups).map((value) => {
          const group = record(value);
          return {
            name: text(group.name),
            type: text(group.type),
            now: text(group.now),
            nodes: strings(group.nodes),
          };
        }),
      };
    },
    async proxyPool(): Promise<RegisterProxyPool> {
      assertCurrent();
      return readRegisterProxyPool(await client.get(`${PATH}/proxy-pool`, { signal }));
    },
    async start(channel: string, accounts: string) {
      assertCurrent();
      const result = record(await client.post(`${PATH}/runs`, { channel, accounts }, { signal }));
      return readRegisterRun(result.run);
    },
    async action(run: string, action: RunAction) {
      assertCurrent();
      await client.post(`${PATH}/runs/${encodeURIComponent(run)}/actions`, { action }, { signal });
    },
    async submitKey(run: string, rowId: string, key: string) {
      assertCurrent();
      await client.post(
        `${PATH}/runs/${encodeURIComponent(run)}/keys`,
        { row_id: rowId, key },
        { signal }
      );
    },
    async probeGateway() {
      assertCurrent();
      await client.post(`${PATH}/gateway/probe`, {}, { signal });
    },
    async saveProxyPool(settings: RegisterProxyPoolSettings) {
      assertCurrent();
      return readRegisterProxyPool(
        await client.put(`${PATH}/proxy-pool`, writeProxyPool(settings), { signal })
      );
    },
    async refreshProxyPool() {
      assertCurrent();
      return readRegisterProxyPool(await client.post(`${PATH}/proxy-pool/refresh`, {}, { signal }));
    },
    async saveChannel(channel: RegisterChannel) {
      assertCurrent();
      await client.put(
        `${PATH}/channels/${encodeURIComponent(channel.id)}`,
        writeChannel(channel),
        { signal }
      );
    },
    async events(
      after: number,
      accept: (event: RegisterEvent) => void,
      reset: (cursor: number) => void,
      connected: () => void
    ) {
      assertCurrent();
      const response = await client.getRaw(`${PATH}/events`, {
        signal,
        adapter: 'fetch',
        responseType: 'stream',
        timeout: 0,
        params: { after },
        headers: { Accept: 'text/event-stream', 'Last-Event-ID': String(after) },
      });
      assertCurrent();
      const stream: unknown = response.data;
      if (!stream || typeof stream !== 'object' || !('getReader' in stream))
        throw new Error('Event stream unavailable');
      connected();
      await consumeEventStream(stream as ReadableStream<Uint8Array>, signal, (frame) => {
        if (!isCurrent()) return;
        let value: unknown;
        try {
          value = JSON.parse(frame.data);
        } catch {
          throw new Error('Invalid event data');
        }
        if (frame.event === 'reset') {
          reset(number(record(value).cursor));
          return;
        }
        const event = readRegisterEvent(value);
        if (event) accept(event);
      });
    },
  };
}
