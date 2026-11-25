import { useCallback, useEffect, useRef, useState } from 'react';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { createRegisterbotClient } from '@/services/api/registerbot';
import { useConfigStore } from '@/stores';
import { waitForReconnect } from './stream';
import type {
  RegisterChannel,
  RegisterEvent,
  RegisterGateway,
  RegisterSnapshot,
  RunAction,
  StreamState,
} from './types';

type Client = ReturnType<typeof createRegisterbotClient>;

export function useRegisterbot() {
  const clientRef = useRef<Client | null>(null);
  const loadVersion = useRef(0);
  const busyRef = useRef(false);
  const [snapshot, setSnapshot] = useState<RegisterSnapshot | null>(null);
  const [gateway, setGateway] = useState<RegisterGateway | null>(null);
  const [events, setEvents] = useState<RegisterEvent[]>([]);
  const [stream, setStream] = useState<StreamState>('connecting');
  const [error, setError] = useState(false);
  const [busy, setBusy] = useState(false);

  const refresh = useCallback(async () => {
    const client = clientRef.current;
    if (!client?.isCurrent()) return;
    const version = ++loadVersion.current;
    try {
      const value = await client.snapshot();
      if (!client.isCurrent() || version !== loadVersion.current) return;
      setSnapshot(value);
      setError(false);
    } catch {
      if (client.isCurrent() && version === loadVersion.current) setError(true);
    }
  }, []);

  const refreshGateway = useCallback(async () => {
    const client = clientRef.current;
    if (!client?.isCurrent()) return;
    try {
      const value = await client.gateway();
      if (client.isCurrent()) setGateway(value);
    } catch {
      if (client.isCurrent()) setGateway(null);
    }
  }, []);

  const reload = useCallback(async () => {
    await Promise.all([refresh(), refreshGateway()]);
  }, [refresh, refreshGateway]);
  useHeaderRefresh(reload);

  useEffect(() => {
    const controller = new AbortController();
    const client = createRegisterbotClient(() => clientRef.current === client, controller.signal);
    clientRef.current = client;
    let cursor = 0;
    let failures = 0;
    let refreshTimer: ReturnType<typeof setTimeout> | undefined;
    const scheduleRefresh = () => {
      if (refreshTimer !== undefined) return;
      refreshTimer = setTimeout(() => {
        refreshTimer = undefined;
        if (client.isCurrent()) void refresh();
      }, 180);
    };
    void reload();
    const watch = async () => {
      while (client.isCurrent()) {
        try {
          await client.events(
            cursor,
            (event) => {
              if (!client.isCurrent() || event.seq <= cursor) return;
              cursor = event.seq;
              setEvents((previous) => [...previous, event].slice(-300));
              scheduleRefresh();
              if (event.stage === 'done' || event.stage === 'writing_key') {
                useConfigStore.getState().clearCache('openai-compatibility');
              }
              if (event.stage.includes('gateway') || event.stage === 'pinning')
                void refreshGateway();
            },
            (nextCursor) => {
              cursor = nextCursor;
              setEvents([]);
              void refresh();
            },
            () => {
              if (client.isCurrent()) {
                failures = 0;
                setStream('live');
              }
            }
          );
        } catch {
          // A dropped connection resumes from the last accepted event, never a new batch.
        }
        if (!client.isCurrent()) return;
        setStream('reconnecting');
        await refresh();
        await waitForReconnect(controller.signal, Math.min(1000 * 2 ** failures++, 15000));
      }
    };
    void watch();
    return () => {
      controller.abort();
      if (refreshTimer !== undefined) clearTimeout(refreshTimer);
      if (clientRef.current === client) clientRef.current = null;
    };
  }, [refresh, refreshGateway, reload]);

  const mutate = useCallback(
    async (operation: (client: Client) => Promise<unknown>) => {
      const client = clientRef.current;
      if (!client?.isCurrent() || busyRef.current) return false;
      busyRef.current = true;
      setBusy(true);
      setError(false);
      try {
        await operation(client);
        if (!client.isCurrent()) return false;
        await refresh();
        return true;
      } catch {
        if (client.isCurrent()) setError(true);
        return false;
      } finally {
        busyRef.current = false;
        if (client.isCurrent()) setBusy(false);
      }
    },
    [refresh]
  );

  return {
    snapshot,
    gateway,
    events,
    stream,
    error,
    busy,
    reload,
    start: (channel: string, accounts: string) =>
      mutate((client) => client.start(channel, accounts)),
    action: (run: string, action: RunAction) => mutate((client) => client.action(run, action)),
    submitKey: (run: string, row: string, key: string) =>
      mutate((client) => client.submitKey(run, row, key)),
    saveChannel: (channel: RegisterChannel) => mutate((client) => client.saveChannel(channel)),
    probeGateway: async () => {
      if (await mutate((client) => client.probeGateway())) await refreshGateway();
    },
  };
}
