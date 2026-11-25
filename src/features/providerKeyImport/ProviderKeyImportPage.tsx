import { useCallback, useEffect, useId, useMemo, useRef, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { usePageTransitionLayer } from '@/components/common/PageTransitionLayer';
import { Button } from '@/components/ui/Button';
import { Card } from '@/components/ui/Card';
import { EmptyState } from '@/components/ui/EmptyState';
import { IconUpload } from '@/components/ui/icons';
import { Select } from '@/components/ui/Select';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { apiClient } from '@/services/api/client';
import { createProviderKeyImportClient } from '@/services/api/providerKeyImport';
import { useAuthStore, useConfigStore } from '@/stores';
import {
  findImportDestinations,
  importDestinationLabel,
  MAX_KEY_IMPORT_ROWS,
  runProviderKeyImport,
  type ImportDestination,
  type ImportStage,
  type ImportUpdate,
} from './batch';
import styles from './ProviderKeyImportPage.module.scss';

type DestinationList =
  { state: 'loading' | 'error' } | { state: 'ready'; items: ImportDestination[] };

type TimelineEntry = { stage: ImportStage; at: number };

function ImportWorkbench() {
  const { t, i18n } = useTranslation();
  const id = useId();
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const loadRef = useRef<AbortController | null>(null);
  const importRef = useRef<AbortController | null>(null);
  const [destinations, setDestinations] = useState<DestinationList>({ state: 'loading' });
  const [selection, setSelection] = useState('');
  const [update, setUpdate] = useState<ImportUpdate | null>(null);
  const [timeline, setTimeline] = useState<TimelineEntry[]>([]);
  const [hasInput, setHasInput] = useState(false);
  const busy = update !== null && update.stage !== 'finished';

  const resetDraft = useCallback(() => {
    if (textareaRef.current) textareaRef.current.value = '';
    setHasInput(false);
    setUpdate(null);
    setTimeline([]);
  }, []);

  const load = useCallback(async () => {
    if (importRef.current) return;
    loadRef.current?.abort();
    const controller = new AbortController();
    loadRef.current = controller;
    const client = createProviderKeyImportClient(
      () => loadRef.current === controller,
      controller.signal
    );
    resetDraft();
    setSelection('');
    setDestinations({ state: 'loading' });
    try {
      const { groups } = await client.read();
      if (!client.isCurrent()) return;
      const items = findImportDestinations(groups);
      setDestinations({ state: 'ready', items });
      if (items.length === 1) setSelection('0');
    } catch {
      if (client.isCurrent()) setDestinations({ state: 'error' });
    } finally {
      if (loadRef.current === controller) loadRef.current = null;
    }
  }, [resetDraft]);

  useHeaderRefresh(load);
  useEffect(() => {
    void load();
    return () => {
      loadRef.current?.abort();
      importRef.current?.abort();
    };
  }, [load]);

  const options = useMemo(
    () =>
      destinations.state === 'ready'
        ? destinations.items.map((item, index) => ({
            value: String(index),
            label: importDestinationLabel(item),
          }))
        : [],
    [destinations]
  );
  const target =
    destinations.state === 'ready' && selection !== ''
      ? destinations.items[Number(selection)]?.target
      : undefined;
  const total = update?.rows.length ?? 0;
  const completed = update?.rows.filter(({ status }) => status !== 'pending').length ?? 0;
  const stage = update?.stage ?? 'idle';

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (importRef.current || !target || !textareaRef.current) return;
    const source = textareaRef.current.value;
    if (!source.trim()) return;
    resetDraft();
    const controller = new AbortController();
    importRef.current = controller;
    const client = createProviderKeyImportClient(
      () => importRef.current === controller,
      controller.signal
    );
    try {
      await runProviderKeyImport(client, target, source, (next) => {
        setUpdate(next);
        const at = Date.now();
        setTimeline((previous) => [...previous, { stage: next.stage, at }]);
        if (
          next.rows.some(
            ({ status }) =>
              status === 'added' || status === 'confirmed_present' || status === 'unknown'
          )
        ) {
          useConfigStore.getState().clearCache('openai-compatibility');
        }
      });
    } finally {
      if (importRef.current === controller) importRef.current = null;
    }
  };

  return (
    <div className={styles.workspace}>
      <Card className={styles.card} title={t('provider_key_import.input_title')}>
        {destinations.state === 'loading' ? (
          <p role="status" className={styles.hint}>
            {t('provider_key_import.loading')}
          </p>
        ) : destinations.state === 'error' ? (
          <EmptyState
            title={t('provider_key_import.load_failed')}
            action={
              <Button variant="secondary" size="sm" onClick={() => void load()}>
                {t('provider_key_import.reload')}
              </Button>
            }
          />
        ) : options.length === 0 ? (
          <EmptyState
            title={t('provider_key_import.no_destinations')}
            description={t('provider_key_import.no_destinations_hint')}
          />
        ) : (
          <form onSubmit={(event) => void submit(event)}>
            <div className="form-group">
              <label htmlFor={`${id}-destination`}>{t('provider_key_import.destination')}</label>
              <Select
                id={`${id}-destination`}
                value={selection}
                options={options}
                placeholder={t('provider_key_import.choose_destination')}
                disabled={busy}
                onChange={(value) => {
                  resetDraft();
                  setSelection(value);
                }}
              />
            </div>
            <div className="form-group">
              <label htmlFor={`${id}-keys`}>{t('provider_key_import.keys')}</label>
              <textarea
                id={`${id}-keys`}
                ref={textareaRef}
                className={styles.keys}
                rows={7}
                autoComplete="off"
                autoCapitalize="off"
                spellCheck={false}
                disabled={!target || busy}
                aria-describedby={`${id}-hint`}
                onInput={(event) => setHasInput(Boolean(event.currentTarget.value.trim()))}
              />
              <p id={`${id}-hint`} className="hint">
                {t('provider_key_import.key_hint', { limit: MAX_KEY_IMPORT_ROWS })}
              </p>
            </div>
            <Button type="submit" loading={busy} disabled={!target || !hasInput}>
              {t('provider_key_import.import')}
            </Button>
          </form>
        )}
      </Card>
      <Card className={styles.card} title={t('provider_key_import.progress_title')}>
        <p className={styles.stage}>
          <span className="pill">{t(`provider_key_import.stage.${stage}`)}</span>
          {total > 0 && (
            <span className={styles.hint}>
              {t('provider_key_import.completed', { done: completed, total })}
            </span>
          )}
        </p>
        {total > 0 && (
          <progress
            className={styles.progress}
            max={total}
            value={completed}
            aria-label={t('provider_key_import.progress_title')}
          />
        )}
        {update?.error && (
          <div className="error-box" role="alert">
            {t(`provider_key_import.error.${update.error}`)}
          </div>
        )}
        <ol className={styles.timeline} role="log" aria-label={t('provider_key_import.timeline')}>
          {timeline.map(({ stage: entryStage, at }) => (
            <li key={entryStage}>
              <time dateTime={new Date(at).toISOString()}>
                {new Date(at).toLocaleTimeString(i18n.language)}
              </time>
              <span>{t(`provider_key_import.stage.${entryStage}`)}</span>
            </li>
          ))}
        </ol>
        {update && total > 0 && (
          <ul className={styles.rows} aria-label={t('provider_key_import.rows')}>
            {update.rows.map(({ line, status }) => (
              <li key={line}>
                <span>{t('provider_key_import.line', { line })}</span>
                <span className={styles.result} data-status={status}>
                  {t(`provider_key_import.row.${status}`)}
                </span>
              </li>
            ))}
          </ul>
        )}
      </Card>
    </div>
  );
}

export function ProviderKeyImportPage() {
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
        <IconUpload size={24} />
        <div>
          <h1>{t('provider_key_import.title')}</h1>
          <p>{t('provider_key_import.description')}</p>
        </div>
      </header>
      {current &&
        (revision >= 0 ? (
          <ImportWorkbench key={revision} />
        ) : (
          <EmptyState title={t('notification.connection_required')} />
        ))}
    </div>
  );
}
