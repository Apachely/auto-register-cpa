import { describe, expect, test } from 'bun:test';
import {
  computeApiUrl,
  normalizeApiBase,
  resolveApiBaseFromLocation,
} from '../src/utils/connection';
import { resolvePluginAssetURL } from '../src/features/plugins/pluginResources';

describe('v8-only management connections', () => {
  test.each([
    ['localhost:8317', 'http://localhost:8317'],
    [' https://proxy.example/ ', 'https://proxy.example'],
    ['https://proxy.example/v8/management/', 'https://proxy.example'],
    ['https://proxy.example/gateway/v8/management///', 'https://proxy.example/gateway'],
  ])('normalizes %s without changing the deployment prefix', (input, expected) => {
    expect(normalizeApiBase(input)).toBe(expected);
    expect(computeApiUrl(input)).toBe(`${expected}/v8/management`);
  });

  test('does not create a URL for empty input', () => {
    expect(computeApiUrl('  ')).toBe('');
  });

  test('does not silently adapt a legacy management URL', () => {
    expect(normalizeApiBase('https://proxy.example/v0/management')).toBe(
      'https://proxy.example/v0/management'
    );
  });

  test('points local Vite development and preview pages at the CPA backend', () => {
    for (const port of ['5173', '4173']) {
      expect(resolveApiBaseFromLocation({ protocol: 'http:', hostname: '127.0.0.1', port })).toBe(
        'http://127.0.0.1:8317'
      );
    }
    expect(
      resolveApiBaseFromLocation({ protocol: 'http:', hostname: 'localhost', port: '5173' })
    ).toBe('http://localhost:8317');
  });

  test('keeps deployed and backend origins unchanged', () => {
    expect(
      resolveApiBaseFromLocation({ protocol: 'https:', hostname: 'proxy.example', port: '5173' })
    ).toBe('https://proxy.example:5173');
    expect(
      resolveApiBaseFromLocation({ protocol: 'http:', hostname: '127.0.0.1', port: '8317' })
    ).toBe('http://127.0.0.1:8317');
  });

  test('preserves backend-declared plugin resource and custom extension paths', () => {
    for (const path of ['/v0/resource/plugins/example/index.html', '/v0/management/example']) {
      expect(resolvePluginAssetURL(path, 'https://proxy.example/v8/management')).toBe(
        `https://proxy.example${path}`
      );
    }
  });
});
