#!/usr/bin/env node
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { execFileSync } = require('node:child_process');

const controller = 'http://127.0.0.1:11461';
const target = 'https://accounts.google.com/generate_204';
const outputPath = path.join(__dirname, 'healthy-nodes.json');
const concurrency = 24;
const maximumNodes = 8;
const minimumNodes = 3;

async function requestJSON(url, timeout = 12000) {
  const response = await fetch(url, { signal: AbortSignal.timeout(timeout) });
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  return response.json();
}

const wait = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

async function selectNode(name) {
  const response = await fetch(`${controller}/proxies/${encodeURIComponent('节点选择')}`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ name }),
    signal: AbortSignal.timeout(3000),
  });
  if (!response.ok) throw new Error(`selector HTTP ${response.status}`);
  // Node tunnels jitter up to several seconds; give the pin time to take effect
  // before reading the exit address, otherwise a healthy node looks unverifiable.
  await wait(800);
}

function readExitIP() {
  const payload = execFileSync(
    'curl.exe',
    [
      '--silent',
      '--show-error',
      '--fail',
      '--proxy',
      'http://127.0.0.1:11460',
      '--connect-timeout',
      '6',
      '--max-time',
      '15',
      'http://api.ipify.org?format=json',
    ],
    { encoding: 'utf8', windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] }
  );
  const address = JSON.parse(payload).ip;
  if (!/^\d{1,3}(?:\.\d{1,3}){3}$/.test(address)) throw new Error('invalid exit IP');
  return address;
}

async function main() {
  const previous = fs.existsSync(outputPath)
    ? JSON.parse(fs.readFileSync(outputPath, 'utf8'))
    : null;
  const previousValid =
    previous?.version === 1 &&
    Date.now() - Number(previous.checked_at || 0) < 24 * 60 * 60 * 1000 &&
    Array.isArray(previous.nodes) &&
    previous.nodes.filter((node) => /^\d{1,3}(?:\.\d{1,3}){3}$/.test(node.exit_ip)).length >=
      minimumNodes;
  const document = await requestJSON(`${controller}/proxies`);
  const selector = document.proxies?.['节点选择'];
  if (!selector || !Array.isArray(selector.all)) throw new Error('dedicated selector not found');
  const nodes = selector.all
    .filter((name) => name && name !== 'DIRECT')
    .sort((left, right) => {
      const score = (name) =>
        (/IEPL/i.test(name) ? 4 : 0) +
        (/新加坡|香港|台湾|日本/.test(name) ? 2 : 0) -
        (/中转|直连/.test(name) ? 2 : 0);
      return score(right) - score(left);
    });
  let cursor = 0;
  const healthy = [];
  const worker = async () => {
    while (cursor < nodes.length) {
      const index = cursor++;
      const node = nodes[index];
      const endpoint = `${controller}/proxies/${encodeURIComponent(node)}/delay?url=${encodeURIComponent(target)}&timeout=9000`;
      try {
        const result = await requestJSON(endpoint, 12000);
        const delay = Number(result.delay);
        if (Number.isFinite(delay) && delay > 0) healthy.push({ name: node, delay_ms: delay });
      } catch {
        // An unhealthy node is omitted; no selector state is changed.
      }
    }
  };
  await Promise.all(Array.from({ length: Math.min(concurrency, nodes.length) }, worker));
  healthy.sort((left, right) => left.delay_ms - right.delay_ms);
  const selected = [];
  const exits = new Set();
  for (const candidate of healthy) {
    try {
      await selectNode(candidate.name);
      const exitIP = readExitIP();
      if (exits.has(exitIP)) continue;
      exits.add(exitIP);
      selected.push({ ...candidate, exit_ip: exitIP });
      if (selected.length >= maximumNodes) break;
    } catch {
      // Google reachability without a confirmed exit is not schedulable.
    }
  }
  if (selected.length < minimumNodes) {
    if (previousValid) {
      process.stdout.write(
        `health refresh found ${selected.length} nodes; retained previous ${previous.nodes.length}-node snapshot\n`
      );
      return;
    }
    throw new Error(`only ${selected.length} Google-capable nodes found`);
  }
  await selectNode(selected[0].name);
  fs.writeFileSync(
    outputPath,
    `${JSON.stringify({ version: 1, checked_at: Date.now(), target, nodes: selected }, null, 2)}\n`,
    'utf8'
  );
  process.stdout.write(`healthy registerbot nodes: ${selected.length}/${nodes.length}\n`);
}

main().catch((error) => {
  process.stderr.write(`registerbot node health check failed: ${error.message}\n`);
  process.exit(1);
});
