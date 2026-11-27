# Registerbot proxy pool requirements and design

## First principles

Registration traffic already has one authoritative forwarding boundary: the
registerbot gateway. The proxy pool must not replace that boundary or allow a
browser, credit check, or API-key verification request to bypass its controller,
forwarding URL, leases, or fallback policy.

A proxy name is not an exit identity. Multiple gateway nodes can resolve to the
same public IP, so allocation and quota decisions must use the exit IP observed
through the forwarding plane after the controller confirms a node pin.

An account is usable only when both business conditions are true:

1. the provider confirms the initial `$5` welcome credit; and
2. the captured API key passes authenticated inference verification and is
   confirmed in the CPA provider configuration.

Node assignment, key capture, key import, and quota accounting therefore form a
small transaction. A crash or retry must never register the same account again
after key capture, double-count an IP, or move a retained browser session to a
different exit.

## Requirements

- Discover candidates from the existing gateway controller and channel group.
- Measure node reachability against the live controller while no account is in
  flight. A generated health snapshot may only seed measured exit addresses, must
  expire, and must never narrow the selectable pool or block startup.
- Keep all browser and API traffic on the existing gateway forwarding URL.
- Allocate a different node and a different measured exit IP for each new
  account; reject an allocation when no distinct verified exit is available.
- Reserve capacity before registration so concurrent workers cannot exceed a
  per-IP quota.
- Hard-bound the configurable quota to at most three usable accounts per exit
  IP.
- Commit quota only after `$5` credit confirmation and successful key import.
- Persist node observations, reservations, completions, and IP counters using an
  atomic replace; store no passwords, cookies, or API keys in the pool ledger.
- Keep reservations across pause, manual handoff, pending import, and process
  restart. Resume on the same node and exit.
- Roll back in-memory quota mutations if persistence fails, retaining the key for
  an idempotent retry.
- Expose policy, health, node state, and IP quota data through authenticated v8
  management endpoints and the existing Registration bot page.

## Runtime flow

1. Refresh the gateway snapshot and resolve the channel's selectable nodes.
2. Choose candidates in round-robin order over a measured-first ordering: nodes
   with a current exit observation, then unmeasured nodes, then cooling nodes last.
3. Pin a candidate through the gateway and verify the controller selection.
4. Read the exit IP through the gateway forwarding plane.
5. Reject aliases of the previous exit and IPs whose usable count plus active
   reservations reaches the configured limit.
6. Persist a row reservation before opening the browser.
7. Hold the gateway route lease through registration and key import.
8. After confirmed provider writeback, atomically record either a usable account
   (`$5` confirmed) or a completed non-usable account (`$5` not confirmed).
9. Release the route lease. Remove the reservation only on terminal abandonment
   or successful quota-ledger completion.

## Management API

All endpoints use the existing `/v8/management/registerbot` authentication.

- `GET /proxy-pool`: policy, counters, node state, and IP ledger.
- `PUT /proxy-pool`: mode, maximum usable accounts per IP, and failure cooldown.
- `POST /proxy-pool/refresh`: refresh selectable nodes from the gateway and sweep
  node reachability into the ledger.

## Verification coverage

Unit tests cover exit rotation, alias-node rejection, quota exhaustion, durable
and idempotent completion, non-usable account accounting, reservation recovery,
ledger-write rollback, controller degradation, settings bounds, queue metadata,
management request validation, read-only node sweeps that never write selector
state, reachability promotion and cooldown with an injected clock, measured-first
candidate ordering, and the degraded paths of an absent, malformed, expired or
empty health snapshot. Browser/provider behavior remains covered by
the existing local CDP fixtures; no external registration is required for these
tests.
