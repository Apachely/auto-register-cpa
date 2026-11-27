import type { ApiError } from '@/types';
import { isRecord } from '@/utils/helpers';

export const INVALID_MANAGEMENT_RESPONSE_CODE = 'invalid_management_response';

export const createInvalidManagementResponseError = (details?: unknown): ApiError => {
  const error = new Error('Invalid Management API response') as ApiError;
  error.name = 'ApiError';
  error.code = 'ERR_BAD_RESPONSE';
  error.apiCode = INVALID_MANAGEMENT_RESPONSE_CODE;
  error.details = details;
  return error;
};

export const requireManagementRecord = (value: unknown): Record<string, unknown> => {
  if (!isRecord(value)) throw createInvalidManagementResponseError();
  return value;
};

export const isHTMLResponse = (value: unknown, contentType: string | null): boolean => {
  if (contentType?.toLowerCase().includes('text/html')) return true;
  if (typeof value !== 'string') return false;
  const prefix = value.trimStart().slice(0, 256).toLowerCase();
  return prefix.startsWith('<!doctype html') || prefix.startsWith('<html');
};
