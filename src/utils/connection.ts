import { DEFAULT_API_PORT, MANAGEMENT_API_PREFIX } from './constants';

const LOCAL_DEVELOPMENT_HOSTS = new Set(['127.0.0.1', 'localhost']);
const LOCAL_DEVELOPMENT_FRONTEND_PORTS = new Set(['4173', '5173']);

interface ApiLocation {
  protocol: string;
  hostname: string;
  port: string;
}

export const normalizeApiBase = (input: string): string => {
  let base = (input || '').trim();
  if (!base) return '';
  base = base.replace(/\/+$/i, '');
  base = base.replace(/\/v8\/management$/i, '');
  if (!/^https?:\/\//i.test(base)) {
    base = `http://${base}`;
  }
  return base;
};

export const computeApiUrl = (base: string): string => {
  const normalized = normalizeApiBase(base);
  if (!normalized) return '';
  return `${normalized}${MANAGEMENT_API_PREFIX}`;
};

export const resolveApiBaseFromLocation = ({ protocol, hostname, port }: ApiLocation): string => {
  const isLocalDevelopmentFrontend =
    LOCAL_DEVELOPMENT_HOSTS.has(hostname) && LOCAL_DEVELOPMENT_FRONTEND_PORTS.has(port);
  const resolvedPort = isLocalDevelopmentFrontend ? String(DEFAULT_API_PORT) : port;
  const normalizedPort = resolvedPort ? `:${resolvedPort}` : '';
  return normalizeApiBase(`${protocol}//${hostname}${normalizedPort}`);
};

export const detectApiBaseFromLocation = (): string => {
  try {
    return resolveApiBaseFromLocation(window.location);
  } catch (error) {
    console.warn('Failed to detect api base from location, fallback to default', error);
    return normalizeApiBase(`http://localhost:${DEFAULT_API_PORT}`);
  }
};
