import { useEffect, useId, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Card } from '@/components/ui/Card';
import { EmptyState } from '@/components/ui/EmptyState';
import type { RegisterProxyPool, RegisterProxyPoolSettings } from './types';
import styles from './RegisterbotPage.module.scss';

interface Props {
  pool: RegisterProxyPool | null;
  busy: boolean;
  onSave: (settings: RegisterProxyPoolSettings) => Promise<boolean>;
  onRefresh: () => Promise<boolean>;
}

function timeLabel(value: number, locale: string) {
  return value > 0 ? new Date(value).toLocaleString(locale) : '—';
}

function PoolStatus({ value }: { value: string }) {
  const { t } = useTranslation();
  return (
    <span className={styles.poolStatus} data-state={value}>
      {t(`registerbot.proxy_pool.status.${value}`, { defaultValue: value })}
    </span>
  );
}

export function ProxyPoolPanel({ pool, busy, onSave, onRefresh }: Props) {
  const { t, i18n } = useTranslation();
  const id = useId();
  const [draft, setDraft] = useState<RegisterProxyPoolSettings | null>(null);

  useEffect(() => {
    if (!pool) return;
    setDraft({
      mode: pool.mode,
      maxUsableAccountsPerIp: pool.maxUsableAccountsPerIp,
      failureCooldownSeconds: pool.failureCooldownSeconds,
    });
  }, [pool]);

  if (!pool || !draft) {
    return (
      <EmptyState
        title={t('registerbot.proxy_pool.unavailable')}
        description={t('registerbot.proxy_pool.unavailable_hint')}
      />
    );
  }

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    await onSave(draft);
  };

  return (
    <section className={styles.poolWorkspace} aria-label={t('registerbot.proxy_pool.title')}>
      <div className={styles.poolToolbar}>
        <div>
          <h2>{t('registerbot.proxy_pool.title')}</h2>
          <p>{t('registerbot.proxy_pool.description')}</p>
        </div>
        <Button variant="secondary" disabled={busy} onClick={() => void onRefresh()}>
          {t('registerbot.proxy_pool.refresh')}
        </Button>
      </div>

      <div className={styles.poolStats}>
        {[
          ['nodes', pool.totalNodes],
          ['ready', pool.readyNodes],
          ['usable', pool.usableAccounts],
          ['exhausted', pool.exhaustedIps],
        ].map(([key, value]) => (
          <div className={styles.poolStat} key={key}>
            <span>{t(`registerbot.proxy_pool.metric.${key}`)}</span>
            <strong>{value}</strong>
          </div>
        ))}
      </div>

      <div className={styles.poolSplit}>
        <Card title={t('registerbot.proxy_pool.settings_title')} className={styles.card}>
          <form onSubmit={(event) => void submit(event)}>
            <div className={styles.poolToggleRow}>
              <div>
                <strong>{t('registerbot.proxy_pool.enforce')}</strong>
                <p>{t('registerbot.proxy_pool.enforce_hint')}</p>
              </div>
              <label className={styles.poolToggle}>
                <input
                  type="checkbox"
                  checked={draft.mode === 'enforce'}
                  disabled={busy}
                  aria-label={t('registerbot.proxy_pool.enforce')}
                  onChange={(event) =>
                    setDraft({ ...draft, mode: event.target.checked ? 'enforce' : 'disabled' })
                  }
                />
                <span />
              </label>
            </div>
            <div className={styles.poolFields}>
              <div className="form-group">
                <label htmlFor={`${id}-max-per-ip`}>{t('registerbot.proxy_pool.max_per_ip')}</label>
                <input
                  id={`${id}-max-per-ip`}
                  className="input"
                  type="number"
                  min={1}
                  max={3}
                  value={draft.maxUsableAccountsPerIp}
                  disabled={busy}
                  onChange={(event) =>
                    setDraft({ ...draft, maxUsableAccountsPerIp: Number(event.target.value) })
                  }
                />
              </div>
              <div className="form-group">
                <label htmlFor={`${id}-cooldown`}>{t('registerbot.proxy_pool.cooldown')}</label>
                <input
                  id={`${id}-cooldown`}
                  className="input"
                  type="number"
                  min={1}
                  max={3600}
                  value={draft.failureCooldownSeconds}
                  disabled={busy}
                  onChange={(event) =>
                    setDraft({ ...draft, failureCooldownSeconds: Number(event.target.value) })
                  }
                />
              </div>
            </div>
            <p className={styles.poolNotice}>{t('registerbot.proxy_pool.criterion')}</p>
            <div className={styles.actions}>
              <Button type="submit" loading={busy}>
                {t('registerbot.proxy_pool.save')}
              </Button>
              <span className={styles.hint}>
                {t('registerbot.proxy_pool.updated')} · {timeLabel(pool.updatedAt, i18n.language)}
              </span>
            </div>
          </form>
        </Card>

        <Card title={t('registerbot.proxy_pool.last_route')} className={styles.card}>
          <dl className={styles.gateway}>
            <dt>{t('registerbot.node')}</dt>
            <dd>{pool.lastNode || '—'}</dd>
            <dt>{t('registerbot.proxy_pool.exit_ip')}</dt>
            <dd className={styles.mono}>{pool.lastExitIp || '—'}</dd>
            <dt>{t('registerbot.proxy_pool.reserved')}</dt>
            <dd>{pool.reserved}</dd>
            <dt>{t('registerbot.proxy_pool.mode')}</dt>
            <dd>{t(`registerbot.proxy_pool.mode_${pool.mode}`)}</dd>
          </dl>
          <p className={styles.hint}>{t('registerbot.proxy_pool.gateway_boundary')}</p>
        </Card>
      </div>

      <Card title={t('registerbot.proxy_pool.nodes_title')} className={styles.card}>
        {pool.nodes.length ? (
          <div className={styles.tableWrap}>
            <table className={styles.poolTable}>
              <thead>
                <tr>
                  <th>{t('registerbot.proxy_pool.node')}</th>
                  <th>{t('registerbot.proxy_pool.exit_ip')}</th>
                  <th>{t('registerbot.proxy_pool.status_label')}</th>
                  <th>{t('registerbot.proxy_pool.quota')}</th>
                  <th>{t('registerbot.proxy_pool.assignments')}</th>
                  <th>{t('registerbot.proxy_pool.failures')}</th>
                  <th>{t('registerbot.proxy_pool.last_used')}</th>
                </tr>
              </thead>
              <tbody>
                {pool.nodes.map((node) => (
                  <tr key={`${node.group}\u0000${node.node}`}>
                    <td>
                      <strong>{node.node}</strong>
                      <span>{node.group}</span>
                    </td>
                    <td className={styles.mono}>{node.exitIp || '—'}</td>
                    <td>
                      <PoolStatus value={node.status} />
                    </td>
                    <td>
                      {node.usableAccounts} / {pool.maxUsableAccountsPerIp}
                      {node.reserved > 0 ? ` (+${node.reserved})` : ''}
                    </td>
                    <td>{node.assignments}</td>
                    <td>{node.failures}</td>
                    <td>{timeLabel(node.lastUsedAt, i18n.language)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <EmptyState
            title={t('registerbot.proxy_pool.no_nodes')}
            description={t('registerbot.proxy_pool.no_nodes_hint')}
          />
        )}
      </Card>

      <Card title={t('registerbot.proxy_pool.ips_title')} className={styles.card}>
        {pool.ips.length ? (
          <div className={styles.tableWrap}>
            <table className={styles.poolTable}>
              <thead>
                <tr>
                  <th>{t('registerbot.proxy_pool.exit_ip')}</th>
                  <th>{t('registerbot.proxy_pool.usable_accounts')}</th>
                  <th>{t('registerbot.proxy_pool.unusable_accounts')}</th>
                  <th>{t('registerbot.proxy_pool.reserved')}</th>
                  <th>{t('registerbot.proxy_pool.remaining')}</th>
                  <th>{t('registerbot.proxy_pool.last_success')}</th>
                </tr>
              </thead>
              <tbody>
                {pool.ips.map((ip) => (
                  <tr key={ip.exitIp}>
                    <td className={styles.mono}>{ip.exitIp}</td>
                    <td>{ip.usableAccounts}</td>
                    <td>{ip.unusableAccounts}</td>
                    <td>{ip.reserved}</td>
                    <td>{ip.remaining}</td>
                    <td>{timeLabel(ip.lastSuccessAt, i18n.language)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <EmptyState title={t('registerbot.proxy_pool.no_ips')} />
        )}
      </Card>
    </section>
  );
}
