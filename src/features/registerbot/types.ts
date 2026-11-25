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
}

export interface RegisterRow {
  id: string;
  line: number;
  account: string;
  stage: string;
  attempt: number;
  maskedKey: string;
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

export interface RegisterSnapshot {
  enabled: boolean;
  storageError: boolean;
  channels: RegisterChannel[];
  runs: RegisterRun[];
  cursor: number;
}

export type RunAction = 'pause' | 'resume' | 'retry' | 'cancel';
export type StreamState = 'connecting' | 'live' | 'reconnecting';
