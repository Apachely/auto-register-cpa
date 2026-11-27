#!/usr/bin/env node
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { execFileSync } = require('node:child_process');

const botDir = __dirname;
const sourcePath = path.join(botDir, 'config.json');
const runtimePath = path.join(botDir, 'runtime-config.json');
const resolutionCachePath = path.join(botDir, 'resolution-cache.json');
const config = JSON.parse(fs.readFileSync(sourcePath, 'utf8'));
const bindInterface = String(process.env.REGISTERBOT_INTERFACE || '').trim();
const localAddress = String(process.env.REGISTERBOT_LOCAL_ADDRESS || '').trim();
if (!bindInterface) throw new Error('REGISTERBOT_INTERFACE is required');
if (!/^\d{1,3}(?:\.\d{1,3}){3}$/.test(localAddress)) {
  throw new Error('REGISTERBOT_LOCAL_ADDRESS is required');
}

const selector = config.outbounds.find((outbound) => outbound.type === 'selector');
if (!selector?.tag) throw new Error('registerbot config has no selector outbound');
const cache = fs.existsSync(resolutionCachePath)
  ? JSON.parse(fs.readFileSync(resolutionCachePath, 'utf8'))
  : { updated_at: 0, hosts: {} };
const cacheFresh = Date.now() - Number(cache.updated_at || 0) < 24 * 60 * 60 * 1000;
const resolved = new Map();

function validIPv4(value) {
  return /^\d{1,3}(?:\.\d{1,3}){3}$/.test(String(value || ''));
}

function resolveIPv4(hostname) {
  if (validIPv4(hostname)) return hostname;
  if (resolved.has(hostname)) return resolved.get(hostname);
  const cached = cache.hosts?.[hostname];
  if (cacheFresh && validIPv4(cached)) {
    resolved.set(hostname, cached);
    return cached;
  }

  let payload;
  try {
    payload = execFileSync(
      'curl.exe',
      [
        '--silent',
        '--show-error',
        '--fail',
        '--noproxy',
        '*',
        '--interface',
        localAddress,
        '--retry',
        '2',
        '--retry-all-errors',
        '--retry-delay',
        '1',
        '--connect-timeout',
        '3',
        '--max-time',
        '6',
        `https://dns.alidns.com/resolve?name=${encodeURIComponent(hostname)}&type=A`,
      ],
      { encoding: 'utf8', windowsHide: true }
    );
  } catch {
    if (validIPv4(cached)) {
      resolved.set(hostname, cached);
      return cached;
    }
    throw new Error(`physical bootstrap DNS failed for ${hostname}`);
  }
  const response = JSON.parse(payload);
  const address = response.Answer?.find(
    (answer) => answer.type === 1 && validIPv4(answer.data)
  )?.data;
  if (!address) throw new Error(`bootstrap DNS returned no IPv4 address for ${hostname}`);
  resolved.set(hostname, address);
  return address;
}

for (const outbound of config.outbounds) {
  if (outbound.type !== 'trojan' || typeof outbound.server !== 'string') continue;
  const hostname = outbound.server;
  outbound.server = resolveIPv4(hostname);
  outbound.bind_interface = bindInterface;
}

config.dns = {
  servers: [
    {
      type: 'https',
      tag: 'registerbot-dns',
      server: '223.5.5.5',
      server_port: 443,
      path: '/dns-query',
      tls: { enabled: true, server_name: 'dns.alidns.com' },
      detour: selector.tag,
    },
  ],
  final: 'registerbot-dns',
  strategy: 'prefer_ipv4',
};
config.route = {
  ...(config.route || {}),
  auto_detect_interface: false,
  default_interface: bindInterface,
};

fs.writeFileSync(runtimePath, `${JSON.stringify(config, null, 2)}\n`, 'utf8');
fs.writeFileSync(
  resolutionCachePath,
  `${JSON.stringify({ version: 1, updated_at: Date.now(), hosts: Object.fromEntries(resolved) }, null, 2)}\n`,
  'utf8'
);
process.stdout.write(
  `runtime config ready: ${runtimePath} (${resolved.size} node hosts resolved, interface ${bindInterface})\n`
);
