#!/usr/bin/env node
// Generates the sing-box config for the registerbot-dedicated proxy instance.
// Reads the subscription URL from SakuraCat's vortex.json at runtime so the
// token never lands in a tracked file. Output goes to registerbot-bot/.
//
// Usage: node bootstrap.cjs [--check-only]
'use strict';
const fs = require('fs');
const path = require('path');
const { execSync } = require('child_process');

const ROOT = path.resolve(__dirname, '..');
const BOT_DIR = __dirname;
const VORTEX = process.env.SAKURA_VORTEX || path.join(process.env.APPDATA || '', 'SakuraCat', 'vortex.json');
const SUB_OVERRIDE = path.join(BOT_DIR, 'subscription-url.txt');
const SING_BOX = path.join(ROOT, '_download', 'singbox', 'sing-box.exe');

const MIXED_PORT = 12460;      // bot browsers and API checks enter here
const CTRL_PORT = 12461;       // bot-only clash-compatible controller

function readSubscriptionURL() {
  if (fs.existsSync(SUB_OVERRIDE)) {
    const u = fs.readFileSync(SUB_OVERRIDE, 'utf8').trim();
    if (u) return u;
  }
  const vortex = JSON.parse(fs.readFileSync(VORTEX, 'utf8'));
  const url = vortex && vortex.subscription_info && vortex.subscription_info.subscription_url;
  if (!url) throw new Error('no subscription URL in vortex.json and no subscription-url.txt override');
  return url;
}

function fetchSubscription(url) {
  // Use curl: PowerShell Invoke-WebRequest mangles large base64 bodies.
  const out = execSync(
    `curl -sL --max-time 30 -A "mihomo/1.18.0" ${JSON.stringify(url)}`,
    { encoding: 'utf8', maxBuffer: 64 * 1024 * 1024 }
  ).trim();
  const decoded = Buffer.from(out, 'base64').toString('utf8');
  return decoded.split(/\r?\n/).filter(Boolean);
}

const JUNK = /剩余流量|距离下次|套餐到期|过期时间|官网|客服|群|重置|流量|GB|到期|expire|traffic|balance/i;

function parseNodes(links) {
  const nodes = [];
  const seen = new Set();
  for (const link of links) {
    const m = /^(\w+):\/\/([^@\s]+)@([^\s:#?]+)(?::(\d+))?([^#]*)?#(.*)$/.exec(link);
    if (!m) continue;
    const name = decodeURIComponent(m[6]).trim();
    if (!name || JUNK.test(name)) continue;
    if (seen.has(name)) continue;
    seen.add(name);

    const password = m[2];
    const host = m[3];
    const port = Number(m[4] || 443);
    const query = new URLSearchParams(m[5] || '');

    nodes.push({
      proto: m[1].toLowerCase(),
      name,
      password,
      host,
      port,
      sni: query.get('sni') || query.get('peer') || host,
      peer: query.get('peer'),
    });
  }
  return nodes;
}

function buildConfig(subURL, nodes) {
  // Convert trojan nodes directly to outbounds; no provider.
  const outbounds = nodes.map((n) => ({
    type: 'trojan',
    tag: n.name,
    server: n.host,
    server_port: n.port,
    password: n.password,
    tls: { enabled: true, insecure: true, server_name: n.sni || n.host },
  }));

  // Add selector and direct/reject
  outbounds.push(
    {
      type: 'selector',
      tag: '节点选择',
      outbounds: nodes.map((n) => n.name).concat(['DIRECT']),
      interrupt_exist_connections: true,
    },
    { type: 'direct', tag: 'DIRECT' },
    { type: 'block', tag: 'REJECT' }
  );

  return {
    log: { level: 'warn', timestamp: true },
    dns: {
      servers: [{ type: 'local', tag: 'local-dns' }],
      rules: [{ action: 'route', server: 'local-dns' }],
      final: 'local-dns',
      strategy: 'prefer_ipv4',
    },
    inbounds: [
      {
        type: 'mixed',
        tag: 'bot-in',
        listen: '127.0.0.1',
        listen_port: MIXED_PORT,
      },
    ],
    outbounds: outbounds,
    route: {
      rules: [{ action: 'route', outbound: '节点选择' }],
    },
    experimental: {
      cache_file: { enabled: true, path: path.join(BOT_DIR, 'cache.db'), cache_id: 'registerbot' },
      clash_api: {
        external_controller: `127.0.0.1:${CTRL_PORT}`,
        external_ui: '',
        secret: '',
        default_mode: 'rule',
      },
    },
  };
}

function main() {
  const checkOnly = process.argv.includes('--check-only');
  const subURL = readSubscriptionURL();
  const links = fetchSubscription(subURL);
  const nodes = parseNodes(links);
  if (nodes.length === 0) throw new Error('subscription decoded but no usable nodes');
  const cfg = buildConfig(subURL, nodes);
  const cfgPath = path.join(BOT_DIR, 'config.json');
  fs.writeFileSync(cfgPath, JSON.stringify(cfg, null, 2));
  console.log(`config written: ${cfgPath}`);
  console.log(`nodes: ${nodes.length} (${new Set(nodes.map((n) => n.host)).size} unique hosts)`);
  console.log(`mixed: 127.0.0.1:${MIXED_PORT}  controller: 127.0.0.1:${CTRL_PORT}`);
  if (checkOnly) {
    if (!fs.existsSync(SING_BOX)) throw new Error('sing-box binary missing at ' + SING_BOX);
    execSync(`"${SING_BOX}" check -c "${cfgPath}" -D "${BOT_DIR}"`, { stdio: 'inherit' });
    console.log('sing-box check: OK');
    return;
  }
  console.log('note: start with start-bot-core.ps1');
}

try {
  main();
} catch (err) {
  console.error('bootstrap failed: ' + err.message);
  process.exit(1);
}
