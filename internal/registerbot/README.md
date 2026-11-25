# Registration engine

`registerbot` adds persistent registration queues to the authenticated v8
management API. The engine is initialized on the first registration API request;
ordinary CPA startup does not launch Chrome. The first supplied driver is
`freeaiapikey`. Multiple channel configurations can use that driver independently.

## Queue and recovery

- Input is one `account|password` pair per line. Empty lines and comments beginning
  with `#` are ignored. Duplicate or malformed rows are reported as skipped.
- A batch accepts up to 500 rows. The engine retains up to 100 runs and permits up
  to 32 configured channels. Channel concurrency is bounded to 1-8 workers and
  attempts to 1-10. The configured interval spaces account starts within a channel.
- Transient driver failures retry up to `max-attempts`. Human verification becomes
  `waiting_human` and does not block other queued rows. Import failures retain the
  captured key in `pending_write`; resuming retries verification and the provider
  write without registering the account again.
- `pause` cancels current work and preserves its checkpoint. `resume` requeues
  unfinished rows; `retry` also requeues failed rows. `cancel` removes unfinished
  secrets and is terminal. Already committed provider keys are not rolled back.
- On restart, unfinished runs are loaded as `paused`. Resuming is deliberate;
  registration never restarts automatically after a process crash.
- `target-keys: 0` processes all rows. A positive target reserves in-flight slots
  to avoid excess automatic registrations. Once enough keys are committed,
  unfinished rows without a captured key are skipped. Captured pending keys remain
  recoverable rather than being discarded.

## Local state

The management adapter stores state in `registerbot/` next to the active CPA
configuration file. It does not currently relocate this directory using the
persisted `state-dir` setting.

| File or directory | Contents |
| --- | --- |
| `settings.yaml` | Module settings and channel definitions; separate from CPA config |
| `queue.enc` | AES-256-GCM encrypted queue credentials and pending captured keys |
| `state.key` | Encryption key; protected with current-user DPAPI on Windows |
| `events.jsonl` | Bounded, sanitized progress journal and replay sequence numbers |
| `profiles/`, `browser-*.json`, `browser-*.pid` | Managed Chrome profiles and ownership records |
| `evidence/` | Sanitized metadata when evidence capture is explicitly requested |

On other platforms the encryption key uses owner-only file permissions. Queue
and settings replacement uses a temporary file, file synchronization, and rename.
Queue ciphertext is authenticated; a missing key or damaged ciphertext stops
initialization rather than silently resetting the queue. Keep the queue and key
together when backing up; Windows DPAPI recovery requires the same user context.

Successful rows clear their password and full captured key. Failed or paused rows
retain the encrypted information needed to retry. Browser profiles and settings
are not encrypted by the queue store; a configured controller secret in
`settings.yaml` remains local configuration data. Public snapshots contain masked
accounts and key hints. Raw authentication pages and screenshots are not written
by the evidence helper.

## Gateway and browser behavior

Gateway fallback is `controller -> proxy_only -> disabled`. Discovery tries an
explicit controller first, then candidates derived from the forwarding port; the
default proxy is `http://127.0.0.1:12450`, whose candidates include port `12440`.
Controller failure keeps forwarding through the proxy. Forwarding failure selects
direct connections. Manual keys and pending imports are probed again before
verification, so they also use this fallback and can recover a restored proxy.

A single Clash-compatible forwarding port has global selector state. The gateway
therefore holds **one exclusive selector lease across all channels** for each
automatic registration and its initial import. This serializes that portion of
work even when channel concurrency is higher. `share-mode` does not create
simultaneous independent per-channel exits. Node selection is confirmed against
the controller; an observed exit IP does not establish that a node has an exclusive
IP or prevent provider account restrictions. Human handoff releases the lease;
resume attempts to pin the retained session's previous node again.

Browser fallback is `embedded -> attach -> system -> manual`. Embedded and system
modes use an installed Chrome/Chromium/Edge binary with an isolated profile, not a
bundled browser download. A bounded pool reuses managed processes; each account
gets a separate disposable browser context with its own forwarding setting.
Attach mode requires a configured DevTools endpoint and does not take ownership
of that browser process. Loopback debugging ports are allocated dynamically.

Manual handoff retains the account page while CPA remains running. Complete the
visible verification step and resume the run, or submit an obtained API key for
verification and import. CAPTCHA, MFA, and other identity checks are not bypassed.
On disconnect, disposable contexts close. A restart can recover a still-running
owned browser only when its persisted browser endpoint UUID matches, but the
account page checkpoint is not restored; unfinished registration resumes from the
driver entry point. Graceful shutdown cancels workers, saves queue state, closes
owned browsers, and disconnects attached browsers.

## Defaults and settings

The generated `settings.yaml` is authoritative. Representative active defaults:

```yaml
enabled: true
gateway:
  proxy-url: http://127.0.0.1:12450
  share-mode: lease
  verify-exit-ip: true
  exit-ip-url: https://api.ipify.org?format=json
  pin-timeout-seconds: 20
  probe-timeout-seconds: 5
browser:
  mode: embedded
  pool-size: 2
  startup-timeout-seconds: 40
  step-timeout-seconds: 120
  retire-after: 8
channels:
  - id: freeaiapikey
    kind: freeaiapikey
    name: FreeAIapikey
    enabled: true
    provider: FreeAi
    proxy-url: http://127.0.0.1:12450
    concurrency: 1
    group: 节点选择
    rotate-node: true
    interval-seconds: 45
    step-delay-millis: 900
    max-attempts: 2
```

Channel updates use the management API and require active account workers to be
paused first. Other file settings are loaded at engine initialization; restart CPA
after editing them. The schema retains some fields without an active runtime
implementation: `gateway.auto-discover`, `gateway.circuit-*`,
`browser.remote-debug-port`, and `browser.human-wait-seconds`. Discovery currently
always considers its candidate list; manual handoff waits for an explicit action.
The current FreeAI management importer uses the existing provider's canonical
endpoint and always verifies, regardless of the schema's `import.*` fields.

## Management API and events

All routes below are relative to `/v8/management/registerbot` and use normal CPA
management authentication. No credentials belong in an event-stream URL.

| Method and path | Contract |
| --- | --- |
| `GET /` | Snapshot: channels, runs, counters, cursor, and storage-error flag |
| `GET /channels` | Channel definitions |
| `PUT /channels/:id` | Replace or create a channel definition |
| `POST /runs` | `{ "channel": "freeaiapikey", "accounts": "..." }`; returns `{ "run": ... }` |
| `POST /runs/:run/actions` | `{ "action": "pause|resume|retry|cancel" }` |
| `POST /runs/:run/keys` | `{ "row_id": "...", "key": "..." }` |
| `GET /gateway` | Last measured gateway status; no live probe |
| `POST /gateway/probe` | Refresh gateway state |
| `GET /events?after=N&channel=ID` | Authenticated SSE; channel filter is optional |

SSE subscriptions capture replay and live subscription atomically. Entries use
monotonic `id`/`seq` values; clients may also send `Last-Event-ID`. A trimmed or
future cursor produces `reset`, followed by retained events. Slow subscribers are
disconnected and must reconnect with their cursor. `ready` confirms subscription
and comment heartbeats keep an otherwise idle connection active. The panel should
refresh its snapshot when replay is reset; events are a bounded journal, not an
unlimited audit history.

Handlers expose fixed error codes such as `invalid_request`, `run_rejected`,
`action_rejected`, `key_rejected`, and `engine_unavailable`, rather than upstream
response bodies. Import stalls distinguish `verification_failed`,
`provider_missing`, `config_conflict`, and `write_failed` when known.

The FreeAI importer requires an existing, uniquely named `FreeAi` provider at
`https://api.freeaiapikey.com/v1`. It checks the key with the models endpoint, then
uses the same v8 configuration GET/PUT and ETag/If-Match contract as
`providerKeyImport`. It preserves unrelated provider fields, avoids duplicate keys,
and confirms the write by reading it back. The stored key entry retains the
channel's `proxy-url`; the verification transport may temporarily be direct when
the gateway has degraded.

## Driver extension and verification

Implement `Driver` (`Kind`, `Run`, and `Probe`), add its kind to
`KnownChannelKinds`, and pass the driver to `NewEngine` or add it to the default
factory. Use `Task.Report` for progress, `Task.OpenSession` for isolated sessions,
and `ErrManualHandoff` for a retained human checkpoint. Return a `Harvest` only
after obtaining the credential. Extend the management import adapter for any new
provider family; it currently supports FreeAI's OpenAI-compatible destination.
Update the panel's stage labels and all seven locales for new public stages.

Use MoeForge JSON/regexp/base64 packages, structured logrus logging, and
credential-acquisition-only time bounds. Never emit credentials or raw provider
responses in events. Focused checks are:

```sh
go test ./internal/registerbot ./internal/api/handlers/management
go vet ./internal/registerbot
go build ./cmd/server
```

Unit tests exercise queue behavior, encrypted recovery, cancellation, gateway and
browser isolation, replay, and provider write protection. They do not establish
Google login success or external key usability. Full-suite and live end-to-end
results must be recorded separately for the actual deployment.
