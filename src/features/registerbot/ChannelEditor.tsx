import { useId, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Select } from '@/components/ui/Select';
import type { RegisterChannel, RegisterGateway } from './types';
import styles from './RegisterbotPage.module.scss';

interface Props {
  channel: RegisterChannel;
  gateway: RegisterGateway | null;
  busy: boolean;
  onSave: (channel: RegisterChannel) => Promise<boolean>;
  onClose: () => void;
}

export function ChannelEditor({ channel, gateway, busy, onSave, onClose }: Props) {
  const { t } = useTranslation();
  const id = useId();
  const [draft, setDraft] = useState(channel);
  const [node, setNode] = useState(channel.nodes[0] ?? '');
  const groupOptions =
    gateway?.groups.map((item) => ({ label: item.name, value: item.name })) ?? [];
  if (draft.group && !groupOptions.some(({ value }) => value === draft.group)) {
    groupOptions.unshift({ label: draft.group, value: draft.group });
  }
  const nodes = gateway?.groups.find((item) => item.name === draft.group)?.nodes ?? draft.nodes;
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (await onSave({ ...draft, nodes: node ? [node] : [] })) onClose();
  };
  return (
    <form className={styles.editor} onSubmit={(event) => void submit(event)}>
      <div className={styles.fields}>
        <div className="form-group">
          <label htmlFor={`${id}-onboarding-role`}>{t('registerbot.onboarding_role')}</label>
          <Select
            id={`${id}-onboarding-role`}
            value={draft.onboardingRole}
            options={[
              { value: '', label: t('registerbot.onboarding_unset') },
              ...['student', 'indie', 'founder', 'employee', 'researcher', 'exploring'].map(
                (value) => ({ value, label: t(`registerbot.role.${value}`) })
              ),
            ]}
            disabled={busy}
            onChange={(value) => setDraft({ ...draft, onboardingRole: value })}
          />
        </div>
        <div className="form-group">
          <label htmlFor={`${id}-onboarding-source`}>{t('registerbot.onboarding_source')}</label>
          <Select
            id={`${id}-onboarding-source`}
            value={draft.onboardingSource}
            options={[
              { value: '', label: t('registerbot.onboarding_unset') },
              ...[
                'google',
                'ai',
                'youtube',
                'instagram',
                'reddit',
                'x',
                'discord',
                'github',
                'friend',
                'other',
              ].map((value) => ({ value, label: t(`registerbot.source.${value}`) })),
            ]}
            disabled={busy}
            onChange={(value) => setDraft({ ...draft, onboardingSource: value })}
          />
        </div>
        {(['id', 'name', 'provider'] as const).map((field) => (
          <div className="form-group" key={field}>
            <label htmlFor={`${id}-${field}`}>{t(`registerbot.channel_${field}`)}</label>
            <input
              id={`${id}-${field}`}
              className="input"
              value={draft[field]}
              required
              maxLength={80}
              disabled={busy || (field === 'id' && Boolean(channel.id))}
              pattern={field === 'id' ? '[a-zA-Z0-9_-]+' : undefined}
              onChange={(event) => setDraft({ ...draft, [field]: event.target.value })}
            />
          </div>
        ))}
        <div className="form-group">
          <label htmlFor={`${id}-concurrency`}>{t('registerbot.concurrency')}</label>
          <input
            id={`${id}-concurrency`}
            type="number"
            min={1}
            max={8}
            className="input"
            value={draft.concurrency}
            disabled={busy}
            onChange={(event) => setDraft({ ...draft, concurrency: Number(event.target.value) })}
          />
        </div>
        <div className="form-group">
          <label htmlFor={`${id}-group`}>{t('registerbot.gateway_group')}</label>
          <Select
            id={`${id}-group`}
            value={draft.group}
            options={groupOptions}
            disabled={busy}
            onChange={(value) => {
              setDraft({ ...draft, group: value });
              setNode('');
            }}
          />
        </div>
        <div className="form-group">
          <label htmlFor={`${id}-node`}>{t('registerbot.node')}</label>
          <Select
            id={`${id}-node`}
            value={node}
            disabled={busy}
            options={[
              { value: '', label: t('registerbot.auto_node') },
              ...nodes.map((value) => ({ value, label: value })),
            ]}
            onChange={setNode}
          />
        </div>
        {(['intervalSeconds', 'maxAttempts', 'targetKeys'] as const).map((field) => (
          <div className="form-group" key={field}>
            <label htmlFor={`${id}-${field}`}>{t(`registerbot.${field}`)}</label>
            <input
              id={`${id}-${field}`}
              type="number"
              min={field === 'maxAttempts' ? 1 : 0}
              max={field === 'maxAttempts' ? 5 : 10000}
              className="input"
              value={draft[field]}
              disabled={busy}
              onChange={(event) => setDraft({ ...draft, [field]: Number(event.target.value) })}
            />
          </div>
        ))}
      </div>
      <p className={styles.hint}>{t('registerbot.onboarding_hint')}</p>
      <div className={styles.actions}>
        <label className={styles.check}>
          <input
            type="checkbox"
            checked={draft.skipOptionalDiscord}
            disabled={busy}
            onChange={(event) => setDraft({ ...draft, skipOptionalDiscord: event.target.checked })}
          />
          {t('registerbot.skip_optional_discord')}
        </label>
        <label className={styles.check}>
          <input
            type="checkbox"
            checked={draft.enabled}
            disabled={busy}
            onChange={(event) => setDraft({ ...draft, enabled: event.target.checked })}
          />
          {t('registerbot.enabled')}
        </label>
        <label className={styles.check}>
          <input
            type="checkbox"
            checked={draft.manualMode}
            disabled={busy}
            onChange={(event) => setDraft({ ...draft, manualMode: event.target.checked })}
          />
          {t('registerbot.manual_mode')}
        </label>
        <Button type="submit" loading={busy}>
          {t('registerbot.save_channel')}
        </Button>
        <Button type="button" variant="secondary" onClick={onClose} disabled={busy}>
          {t('registerbot.close')}
        </Button>
      </div>
    </form>
  );
}
