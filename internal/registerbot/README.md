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
| `proxy-pool.json` | Gateway node observations, exit-IP quota counts and resumable reservations |
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

The registration proxy pool is a scheduler and quota ledger above the existing
gateway, not a second forwarding service. It discovers selectable nodes from the
configured controller, pins one node through the same gateway, verifies the
observed exit IP, and reserves that exit for one account. Consecutive accounts
must use both a different node and a different observed exit. Nodes that resolve
to the previous exit, cannot be verified, or map to a full IP enter a bounded
cooldown. The browser, welcome-credit flow, key verification, and provider write
continue to use the gateway forwarding URL and its existing fallback policy.

`proxy-pool.json` contains no account passwords or API keys. A quota entry is
committed only after the driver observed the $5 welcome credit and the importer
completed authenticated API-key verification plus confirmed CPA configuration
writeback. An imported key without confirmed welcome credit remains completed
but is not a usable account and does not consume the per-IP quota. The maximum is
hard-bounded to three usable accounts per exit IP.
See [PROXY_POOL.md](PROXY_POOL.md) for the derived requirements, transaction
boundaries, and verification coverage.

The FreeAI completion path is: select the configured survey answers, click
`Get my $5` once, observe the provider's saved-survey confirmation, click
`Finish later and go to my dashboard`, and capture the complete `Default Key`.
The defaults are `onboarding-role: student` and `onboarding-source: google`,
including old persisted channels where those fields are empty. The fields remain
configurable. Updating answers takes effect
when a retained task resumes.

On a first visit, `/setup` may appear before its controls render. The driver
waits within the acquisition deadline, recognizes the actual submit control
even when the form uses `display: contents`, and reads back both checked radio
states before submitting. A transient unknown setup state is not an immediate
manual handoff.
Ordinary Google sign-in page timeouts close the stalled context and use the
queue's bounded account retry policy. Explicit CAPTCHA/MFA/security challenges
still preserve the account's manual checkpoint; they are not bypassed.

Survey completion is separate from welcome-credit eligibility. When the provider
says the answers are saved but no reward is available, the driver records that
outcome and still follows Finish later to capture the Default Key. It never
changes routes or retries a reward claim to evade eligibility restrictions.
Model usability remains subject to the provider's actual balance and quotas.

The optional coding-tool, test, Discord and plan tutorials are not required.
The dashboard keeps its `Finish setup` banner after Finish later: confirmed
survey completion plus the Default Key takes precedence over that banner, including on
resume. A changed submit label or DOM redraw never authorizes a second credit
claim. An unconfirmed survey or an unrecognized page retains bounded recovery.
`skip-optional-discord` remains a compatible stored setting but the fast path
does not enter the Discord tutorial.

Dashboard key labels are truncated. Keys are read only from the full literal in
the provider's `Copy key` action inside the named Default Key row, never another
key or the shortened display text. The public model catalog is not an authentication
check: importing requires a successful, bounded-output chat completion using the
destination's first configured model, matching the panel's automatic key test.
A five-token response exhausted during reasoning is accepted only when it has
an assistant role, nonempty reasoning output, `finish_reason: length`, and a
positive completion-token count. Empty output, error bodies, redirects and
public model lists still fail verification.

Gateway fallback is `controller -> proxy_only -> disabled`. Discovery tries an
explicit controller first, then candidates derived from the forwarding port; the
default proxy is `http://127.0.0.1:12450`, whose candidates include port `12440`.
Controller failure keeps forwarding through the proxy. Forwarding failure selects
direct connections. Manual keys and pending imports are probed again before
verification, so they also use this fallback and can recover a restored proxy.

A single Clash-compatible forwarding port has global selector state. Accounts
using the same fixed group/node share one confirmed route lease and run in
parallel without repinning it. A different node, or `rotate-node: true`, waits
for every active holder to release before changing the selector. Degraded
forwarding also permits parallel accounts because it performs no node change.
`share-mode` does not create simultaneous independent per-channel exits. Human
handoff releases the lease; resume pins the retained session's previous node.

The default channel uses two workers, two isolated browser processes, a fixed
route and a two-second start interval. Paste multiple `account|password` lines
to create a batch; the channel concurrency and browser pool remain bounded to
eight. An account that requires an external identity challenge retains its own
checkpoint without blocking the remaining rows. Browser session state is never
shared between accounts.

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
proxy-pool:
  mode: enforce
  max-usable-accounts-per-ip: 3
  failure-cooldown-seconds: 60
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
    concurrency: 2
    group: 节点选择
    rotate-node: false
    interval-seconds: 2
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
| `GET /proxy-pool` | Proxy pool policy, node health and per-IP quota snapshot |
| `PUT /proxy-pool` | Replace proxy pool mode, per-IP limit and failure cooldown |
| `POST /proxy-pool/refresh` | Refresh selectable gateway nodes without registering an account |
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
`https://api.freeaiapikey.com/v1`. It checks the key with an authenticated chat
completion using the first configured model and a five-token output limit, then
uses the same v8 configuration GET/PUT and ETag/If-Match contract as
`providerKeyImport`. It preserves unrelated provider fields, avoids duplicate keys,
and confirms the write by reading it back. The stored key entry retains the
channel's `proxy-url`; the verification transport may temporarily be direct when
the gateway has degraded.

Verification requests run in parallel. After verification, a short commit lock
protects a fresh provider read, append/deduplication, ETag write and confirmation.
The verified model and endpoint must still match the fresh configuration. This
prevents concurrent accounts from overwriting each other's keys while preserving
the existing CAS protection against unrelated configuration edits.

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
node internal/registerbot/testdata/google_identifier.test.cjs
node internal/registerbot/testdata/onboarding_completion.test.cjs
# Optional installed-browser regression; all page requests use local fixtures:
REGISTERBOT_BROWSER_SMOKE=1 go test ./internal/registerbot -run TestFreeAIFreshSurveyBrowser -v -count=1
REGISTERBOT_BROWSER_SMOKE=1 go test ./internal/registerbot -run TestFreeAIParallelSurveyBrowser -v -count=1
```

Unit tests exercise queue behavior, encrypted recovery, cancellation, gateway and
browser isolation, replay, and provider write protection. They do not establish
Google login success or external key usability. Full-suite and live end-to-end
results must be recorded separately for the actual deployment.
