import { useId, useRef, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { usePageTransitionLayer } from '@/components/common/PageTransitionLayer';
import { Button } from '@/components/ui/Button';
import { Card } from '@/components/ui/Card';
import { EmptyState } from '@/components/ui/EmptyState';
import { IconBot } from '@/components/ui/icons';
import { Select } from '@/components/ui/Select';
import { apiClient } from '@/services/api/client';
import { useAuthStore } from '@/stores';
import { ChannelEditor } from './ChannelEditor';
import { useRegisterbot } from './useRegisterbot';
import type { RegisterChannel, RegisterRow, RegisterRun, RunAction } from './types';
import styles from './RegisterbotPage.module.scss';

function Stage({ value }: { value: string }) {
  const { t } = useTranslation();
  return (
    <span className={styles.badge} data-state={value}>
      {t(`registerbot.stage.${value}`, { defaultValue: value })}
    </span>
  );
}

function ManualKey({
  run,
  row,
  busy,
  submitKey,
}: {
  run: string;
  row: RegisterRow;
  busy: boolean;
  submitKey: (run: string, row: string, key: string) => Promise<boolean>;
}) {
  const { t } = useTranslation();
  const id = useId();
  const keyRef = useRef<HTMLInputElement>(null);
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    const key = keyRef.current?.value.trim() ?? '';
    if (!key) return;
    if (keyRef.current) keyRef.current.value = '';
    await submitKey(run, row.id, key);
  };
  return (
    <form className={styles.manual} onSubmit={(event) => void submit(event)}>
      <label htmlFor={id}>{t('registerbot.manual_key')}</label>
      <input
        id={id}
        ref={keyRef}
        className="input"
        type="password"
        autoComplete="new-password"
        spellCheck={false}
        required
        disabled={busy}
        placeholder="sk-…"
      />
      <Button type="submit" size="sm" variant="secondary" disabled={busy}>
        {t('registerbot.verify_import')}
      </Button>
    </form>
  );
}

function RunCard({
  run,
  busy,
  action,
  submitKey,
}: {
  run: RegisterRun;
  busy: boolean;
  action: (run: string, action: RunAction) => Promise<boolean>;
  submitKey: (run: string, row: string, key: string) => Promise<boolean>;
}) {
  const { t, i18n } = useTranslation();
  const active = ['queued', 'running', 'waiting_human', 'paused'].includes(run.status);
  const complete = run.counters.done + run.counters.failed + run.counters.skipped;
  return (
    <article className={styles.run}>
      <div className={styles.runHeader}>
        <div>
          <strong>{run.channel}</strong>
          <p className={styles.mono}>{run.id}</p>
        </div>
        <Stage value={run.status} />
      </div>
      <div className={styles.metrics}>
        {(['total', 'done', 'failed', 'waiting', 'pendingWrite'] as const).map((key) => (
          <div key={key}>
            <span>{t(`registerbot.counter.${key}`)}</span>
            <strong>{run.counters[key]}</strong>
          </div>
        ))}
      </div>
      <progress
        max={Math.max(1, run.counters.total)}
        value={complete}
        className={styles.progress}
        aria-label={t('registerbot.progress', { done: complete, total: run.counters.total })}
      />
      <div className={styles.actions}>
        {active && run.status !== 'paused' && (
          <Button
            size="sm"
            variant="secondary"
            disabled={busy}
            onClick={() => void action(run.id, 'pause')}
          >
            {t('registerbot.pause')}
          </Button>
        )}
        {['paused', 'waiting_human'].includes(run.status) && (
          <Button
            size="sm"
            variant="secondary"
            disabled={busy}
            onClick={() => void action(run.id, 'resume')}
          >
            {t('registerbot.resume')}
          </Button>
        )}
        {(run.counters.failed > 0 || run.counters.pendingWrite > 0) && (
          <Button
            size="sm"
            variant="secondary"
            disabled={busy}
            onClick={() => void action(run.id, 'retry')}
          >
            {t('registerbot.retry')}
          </Button>
        )}
        {active && (
          <Button
            size="sm"
            variant="danger"
            disabled={busy}
            onClick={() => void action(run.id, 'cancel')}
          >
            {t('registerbot.cancel')}
          </Button>
        )}
        <time className={styles.hint}>{new Date(run.createdAt).toLocaleString(i18n.language)}</time>
      </div>
      {run.counters.waiting > 0 && <p className={styles.notice}>{t('registerbot.handoff_hint')}</p>}
      <ul className={styles.rows} aria-label={t('registerbot.accounts')}>
        {run.rows.map((row) => (
          <li key={row.id}>
            <div className={styles.rowHeader}>
              <span className={styles.mono}>
                {row.account || t('registerbot.row', { line: row.line })}
              </span>
              <Stage value={row.stage} />
              <span className={styles.hint}>
                {t('registerbot.attempt', { attempt: row.attempt })}
              </span>
              {row.maskedKey && <span className={styles.mono}>{row.maskedKey}</span>}
            </div>
            {row.reason && (
              <p className={styles.hint}>
                {t(`registerbot.reason.${row.reason}`, { defaultValue: row.reason })}
              </p>
            )}
            {row.stage === 'waiting_human' && (
              <ManualKey run={run.id} row={row} busy={busy} submitKey={submitKey} />
            )}
          </li>
        ))}
      </ul>
    </article>
  );
}

function RegisterbotWorkbench() {
  const { t, i18n } = useTranslation();
  const id = useId();
  const work = useRegisterbot();
  const sourceRef = useRef<HTMLTextAreaElement>(null);
  const [hasInput, setHasInput] = useState(false);
  const [selection, setSelection] = useState('');
  const [editor, setEditor] = useState<RegisterChannel | null>(null);
  const channels = work.snapshot?.channels ?? [];
  const enabled = channels.filter((channel) => channel.enabled);
  const selected = channels.find((channel) => channel.id === selection) ?? enabled[0];
  const options = channels.map((channel) => ({
    value: channel.id,
    label: channel.name || channel.id,
  }));
  const visibleRuns = (work.snapshot?.runs ?? [])
    .filter((run) => !selected || run.channel === selected.id)
    .sort((a, b) => b.createdAt - a.createdAt);
  const visibleEvents = work.events.filter(
    (event) => !selected || !event.channel || event.channel === selected.id
  );
  const latestBrowser = [...visibleEvents].reverse().find((event) => event.browser)?.browser;
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!selected?.enabled || work.busy || !sourceRef.current) return;
    const accounts = sourceRef.current.value;
    if (!accounts.trim()) return;
    sourceRef.current.value = '';
    setHasInput(false);
    await work.start(selected.id, accounts);
  };

  if (!work.snapshot) {
    return work.error ? (
      <EmptyState
        title={t('registerbot.load_failed')}
        description={t('registerbot.backend_required')}
        action={
          <Button variant="secondary" onClick={() => void work.reload()}>
            {t('registerbot.reload')}
          </Button>
        }
      />
    ) : (
      <p role="status" className={styles.hint}>
        {t('registerbot.loading')}
      </p>
    );
  }
  return (
    <div className={styles.workspace}>
      <div className={styles.statusBar} role="status">
        <span className={styles.badge} data-state={work.stream}>
          {t(`registerbot.stream.${work.stream}`)}
        </span>
        <span>
          {t('registerbot.gateway')}:{' '}
          {work.gateway
            ? t(`registerbot.mode.${work.gateway.mode}`, { defaultValue: work.gateway.mode })
            : t('registerbot.unknown')}
        </span>
        <span>
          {t('registerbot.browser')}: {latestBrowser || t('registerbot.browser_on_demand')}
        </span>
        <span className={styles.hint}>{t('registerbot.live_hint')}</span>
      </div>
      {work.error && (
        <div className="error-box" role="alert">
          {t('registerbot.operation_failed')}
        </div>
      )}
      {!work.snapshot.enabled && <p className={styles.notice}>{t('registerbot.disabled')}</p>}
      {work.snapshot.storageError && (
        <p className={styles.notice} role="alert">
          {t('registerbot.storage_error')}
        </p>
      )}
      <div className={styles.columns}>
        <Card className={styles.card} title={t('registerbot.new_batch')}>
          <div className="form-group">
            <label htmlFor={`${id}-channel`}>{t('registerbot.channel')}</label>
            <Select
              id={`${id}-channel`}
              value={selected?.id ?? ''}
              options={options}
              onChange={(value) => {
                setSelection(value);
                setEditor(null);
              }}
              disabled={work.busy}
            />
          </div>
          <div className={styles.actions}>
            <Button
              size="sm"
              variant="secondary"
              disabled={!selected || work.busy}
              onClick={() => selected && setEditor(selected)}
            >
              {t('registerbot.edit_channel')}
            </Button>
            <Button
              size="sm"
              variant="secondary"
              disabled={!channels[0] || work.busy}
              onClick={() => setEditor({ ...channels[0], id: '', name: '', nodes: [] })}
            >
              {t('registerbot.add_channel')}
            </Button>
          </div>
          {editor ? (
            <ChannelEditor
              key={editor.id}
              channel={editor}
              gateway={work.gateway}
              busy={work.busy}
              onSave={work.saveChannel}
              onClose={() => setEditor(null)}
            />
          ) : (
            <form className={styles.batchForm} onSubmit={(event) => void submit(event)}>
              <div className="form-group">
                <label htmlFor={`${id}-accounts`}>{t('registerbot.accounts')}</label>
                <textarea
                  id={`${id}-accounts`}
                  ref={sourceRef}
                  rows={6}
                  className={styles.accounts}
                  disabled={work.busy || !selected?.enabled || !work.snapshot.enabled}
                  autoComplete="off"
                  autoCapitalize="off"
                  spellCheck={false}
                  maxLength={257000}
                  aria-describedby={`${id}-hint`}
                  onInput={(event) => setHasInput(Boolean(event.currentTarget.value.trim()))}
                />
                <p id={`${id}-hint`} className="hint">
                  {t('registerbot.accounts_hint')}
                </p>
              </div>
              <div className={styles.destination}>
                <span>{t('registerbot.destination')}</span>
                <strong>{selected?.provider || '—'}</strong>
                <span className={styles.mono}>{selected?.proxyUrl}</span>
              </div>
              <Button
                type="submit"
                loading={work.busy}
                disabled={!hasInput || !selected?.enabled || !work.snapshot.enabled}
              >
                {t('registerbot.start')}
              </Button>
            </form>
          )}
        </Card>
        <Card className={styles.card} title={t('registerbot.gateway_title')}>
          <div className={styles.actions}>
            <Button
              size="sm"
              variant="secondary"
              disabled={work.busy}
              onClick={() => void work.probeGateway()}
            >
              {t('registerbot.probe_gateway')}
            </Button>
          </div>
          {work.gateway ? (
            <>
              <dl className={styles.gateway}>
                <dt>{t('registerbot.proxy')}</dt>
                <dd>{work.gateway.proxyUrl || '—'}</dd>
                <dt>{t('registerbot.controller')}</dt>
                <dd>{work.gateway.controllerUrl || '—'}</dd>
                <dt>{t('registerbot.sharing')}</dt>
                <dd>
                  {t(`registerbot.mode.${work.gateway.shareMode}`, {
                    defaultValue: work.gateway.shareMode,
                  })}
                </dd>
                <dt>{t('registerbot.gateway_group')}</dt>
                <dd>{selected?.group || '—'}</dd>
                <dt>{t('registerbot.node')}</dt>
                <dd>{selected?.nodes.join(', ') || t('registerbot.auto_node')}</dd>
              </dl>
              <p className={styles.hint}>{t('registerbot.gateway_hint')}</p>
              {work.gateway.warnings.map((warning) => (
                <p key={warning} className={styles.notice}>
                  {warning}
                </p>
              ))}
              {work.gateway.error && <p className={styles.notice}>{work.gateway.error}</p>}
            </>
          ) : (
            <EmptyState
              title={t('registerbot.gateway_unavailable')}
              description={t('registerbot.degrade_hint')}
            />
          )}
          <p className={styles.ladder}>{t('registerbot.ladders')}</p>
        </Card>
      </div>
      <Card title={t('registerbot.runs_title')} className={styles.card}>
        {visibleRuns.length ? (
          visibleRuns.map((run) => (
            <RunCard
              key={run.id}
              run={run}
              busy={work.busy}
              action={work.action}
              submitKey={work.submitKey}
            />
          ))
        ) : (
          <EmptyState
            title={t('registerbot.no_runs')}
            description={t('registerbot.no_runs_hint')}
          />
        )}
      </Card>
      <Card title={t('registerbot.events_title')} className={styles.card}>
        {visibleEvents.length ? (
          <ol
            className={styles.timeline}
            role="log"
            aria-live="polite"
            aria-relevant="additions"
            aria-label={t('registerbot.events_title')}
          >
            {visibleEvents.map((event) => (
              <li key={event.seq} data-level={event.level}>
                <time>{new Date(event.at).toLocaleTimeString(i18n.language)}</time>
                <Stage value={event.stage} />
                <span className={styles.eventText}>
                  {event.message
                    ? t(`registerbot.reason.${event.message}`, { defaultValue: event.message })
                    : event.account}
                </span>
                {event.node && <span className={styles.hint}>{event.node}</span>}
                {event.browser && <span className={styles.hint}>{event.browser}</span>}
              </li>
            ))}
          </ol>
        ) : (
          <EmptyState title={t('registerbot.no_events')} />
        )}
      </Card>
    </div>
  );
}

export function RegisterbotPage() {
  const { t } = useTranslation();
  const current = usePageTransitionLayer()?.isCurrentLayer ?? true;
  const revision = useAuthStore((state) =>
    state.isAuthenticated && state.connectionStatus === 'connected'
      ? apiClient.getConnectionRevision()
      : -1
  );
  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <IconBot size={24} />
        <div>
          <h1>{t('registerbot.title')}</h1>
          <p>{t('registerbot.description')}</p>
        </div>
      </header>
      {current &&
        (revision >= 0 ? (
          <RegisterbotWorkbench key={revision} />
        ) : (
          <EmptyState title={t('notification.connection_required')} />
        ))}
    </div>
  );
}
