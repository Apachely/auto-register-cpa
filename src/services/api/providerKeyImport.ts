import { isRecord } from '@/utils/helpers';
import { apiClient } from './client';
import { readProviderGroups } from './providers';

const FAMILY = 'openai-compatibility';
const PATH = `/config/api-keys/${FAMILY}`;

export function createProviderKeyImportClient(isActive: () => boolean, signal: AbortSignal) {
  const revision = apiClient.getConnectionRevision();
  const isCurrent = () =>
    !signal.aborted && isActive() && revision === apiClient.getConnectionRevision();
  const client = apiClient.captureConnection(isCurrent);

  return {
    isCurrent,
    async read() {
      if (!isCurrent()) throw new Error('Connection changed');
      try {
        const response = await client.getRaw(PATH, { signal });
        return {
          groups: readProviderGroups({ 'api-keys': { [FAMILY]: response.data } }, FAMILY),
          etag: typeof response.headers.etag === 'string' ? response.headers.etag : '',
        };
      } catch (error: unknown) {
        if (
          typeof error === 'object' &&
          error !== null &&
          'status' in error &&
          error.status === 404
        ) {
          return { groups: [], etag: '' };
        }
        throw error;
      }
    },
    async write(groups: Record<string, unknown>[], etag: string) {
      if (!isCurrent()) throw new Error('Connection changed');
      const result = await client.put(PATH, groups, { signal, headers: { 'If-Match': etag } });
      if (!isRecord(result) || result.status !== 'ok' || result['config-version'] !== 8) {
        throw new Error('Unconfirmed configuration write');
      }
    },
  };
}
