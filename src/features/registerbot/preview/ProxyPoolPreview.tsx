import { ProxyPoolPanel } from '../ProxyPoolPanel';
import type { RegisterProxyPool } from '../types';
import styles from '../RegisterbotPage.module.scss';

const fixture: RegisterProxyPool = {
  enabled: true,
  mode: 'enforce',
  maxUsableAccountsPerIp: 3,
  failureCooldownSeconds: 60,
  totalNodes: 4,
  readyNodes: 2,
  reserved: 1,
  exhaustedIps: 1,
  usableAccounts: 7,
  lastNode: 'HK-IEPL 03',
  lastExitIp: '203.0.113.23',
  updatedAt: Date.now(),
  nodes: [
    {
      group: '节点选择',
      node: 'HK-IEPL 03',
      exitIp: '203.0.113.23',
      status: 'reserved',
      usableAccounts: 1,
      reserved: 1,
      assignments: 5,
      failures: 0,
      lastUsedAt: Date.now() - 42_000,
    },
    {
      group: '节点选择',
      node: 'JP-Premium 01',
      exitIp: '198.51.100.41',
      status: 'ready',
      usableAccounts: 2,
      reserved: 0,
      assignments: 4,
      failures: 1,
      lastUsedAt: Date.now() - 180_000,
    },
    {
      group: '节点选择',
      node: 'SG-Edge 02',
      exitIp: '192.0.2.18',
      status: 'exhausted',
      usableAccounts: 3,
      reserved: 0,
      assignments: 6,
      failures: 0,
      lastUsedAt: Date.now() - 420_000,
    },
    {
      group: '节点选择',
      node: 'US-West 04',
      exitIp: '',
      status: 'cooldown',
      usableAccounts: 0,
      reserved: 0,
      assignments: 1,
      failures: 2,
      lastUsedAt: Date.now() - 80_000,
    },
  ],
  ips: [
    {
      exitIp: '192.0.2.18',
      usableAccounts: 3,
      unusableAccounts: 1,
      reserved: 0,
      remaining: 0,
      lastUsedAt: Date.now() - 420_000,
      lastSuccessAt: Date.now() - 460_000,
    },
    {
      exitIp: '198.51.100.41',
      usableAccounts: 2,
      unusableAccounts: 0,
      reserved: 0,
      remaining: 1,
      lastUsedAt: Date.now() - 180_000,
      lastSuccessAt: Date.now() - 180_000,
    },
    {
      exitIp: '203.0.113.23',
      usableAccounts: 1,
      unusableAccounts: 0,
      reserved: 1,
      remaining: 1,
      lastUsedAt: Date.now() - 42_000,
      lastSuccessAt: Date.now() - 86_000,
    },
  ],
};

export function ProxyPoolPreview() {
  return (
    <main className={`${styles.page} ${styles.preview}`}>
      <ProxyPoolPanel
        pool={fixture}
        busy={false}
        onSave={async () => true}
        onRefresh={async () => true}
      />
    </main>
  );
}
