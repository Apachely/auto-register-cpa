export interface RegisterChannel {
  id: string;
  kind: string;
  name: string;
  enabled: boolean;
  provider: string;
  proxyUrl: string;
  concurrency: number;
  group: string;
  nodes: string[];
  rotateNode: boolean;
  intervalSeconds: number;
  stepDelayMillis: number;
  targetKeys: number;
  maxAttempts: number;
  startUrl: string;
  manualMode: boolean;
  onboardingRole: string;
  onboardingSource: string;
  skipOptionalDiscord: boolean;
}

export interface RegisterCounters {
  total: number;
  queued: number;
  running: number;
  done: number;
  failed: number;
  skipped: number;
  waiting: number;
  pendingWrite: number;
  harvested: number;
  usable: number;
}

export interface RegisterRow {
  id: string;
  line: number;
  account: string;
  stage: string;
  attempt: number;
  maskedKey: string;
  node: string;
  exitIp: string;
  creditConfirmed: boolean;
  usable: boolean;
  reason: string;
  updatedAt: number;
}

export interface RegisterRun {
  id: string;
  channel: string;
  status: string;
  createdAt: number;
  updatedAt: number;
  rows: RegisterRow[];
  counters: RegisterCounters;
}

export interface RegisterEvent {
  seq: number;
  at: number;
  channel: string;
  run: string;
  account: string;
  stage: string;
  level: string;
  message: string;
  node: string;
  exitIp: string;
  browser: string;
}

export interface GatewayGroup {
  name: string;
  type: string;
  now: string;
  nodes: string[];
}

export interface RegisterGateway {
  mode: string;
  proxyUrl: string;
  controllerUrl: string;
  shareMode: string;
  groups: GatewayGroup[];
  warnings: string[];
  error: string;
}

export interface RegisterProxyPoolSettings {
  mode: 'disabled' | 'enforce';
  maxUsableAccountsPerIp: number;
  failureCooldownSeconds: number;
}

export interface RegisterProxyPoolNode {
  group: string;
  node: string;
  exitIp: string;
  status: string;
  usableAccounts: number;
  reserved: number;
  assignments: number;
  failures: number;
  lastUsedAt: number;
}

export interface RegisterProxyPoolIp {
  exitIp: string;
  usableAccounts: number;
  unusableAccounts: number;
  reserved: number;
  remaining: number;
  lastUsedAt: number;
  lastSuccessAt: number;
}

export interface RegisterProxyPool extends RegisterProxyPoolSettings {
  enabled: boolean;
  totalNodes: number;
  readyNodes: number;
  reserved: number;
  exhaustedIps: number;
  usableAccounts: number;
  lastNode: string;
  lastExitIp: string;
  updatedAt: number;
  nodes: RegisterProxyPoolNode[];
  ips: RegisterProxyPoolIp[];
}

export interface RegisterSnapshot {
  enabled: boolean;
  storageError: boolean;
  channels: RegisterChannel[];
  runs: RegisterRun[];
  cursor: number;
}

export type RunAction = 'pause' | 'resume' | 'retry' | 'cancel';
export type StreamState = 'connecting' | 'live' | 'reconnecting';
