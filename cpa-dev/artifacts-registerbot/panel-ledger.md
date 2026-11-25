# Panel execution ledger

## Read guidance

- `cpa-panel/AGENTS.md` (read once for this subtask).
- Parent-loaded `fullstack-developer/SKILL.md` and `admin-design-skill/SKILL.md` reused.
- `admin-design-skill/references/tokens.md`, `components.md`, `layouts.md`, and `patterns.md` read once. Explicit CPA design requirements take precedence over generic design defaults.

## Inspected contracts

- Existing providerKeyImport page, SCSS module, API client, batch tests, router, sidebar, Button/Card/Select/EmptyState, theme variables, header refresh hook, error parser, and locale parity tests.
- Backend registerbot event, channel, gateway, engine, and handler DTOs. Confirmed snake_case and millisecond timestamps against current Go code.
- Axios installed adapter source and type declarations confirm fetch streaming support with bearer headers through the existing captured API client.

## Commands and results

- `rg --files ... bun.exe` scoped to known Node cache and Bun install directories located the cached runtime.
- Cached runtime `bun --version`: `1.3.14`; package declaration remains `bun@1.4.3` unchanged.
- Scoped `tsc --noEmit`: initial missing icon and ES2023 `toSorted` findings repaired using the existing `IconBot` and ES2020 array sorting; subsequent type-check passed.
- Scoped ESLint on registerbot feature and API module passed.
- `bun test tests/registerbot.test.ts tests/localeParity.test.ts`: 16 passed, 0 failed.
- Initial `bun run verify`: 2394 passed, 10 failed in pre-existing Windows path/host locale assumptions.
- `bun test tests/quotaTimelineDst.test.ts tests/xaiQuotaUnavailable.test.ts`: 30 passed after test-only portability fixes.
- Full `bun run verify` after implementation: 2404 passed, 0 failed; ESLint, TypeScript, Vite succeeded.
- Full `bun run verify` after storage-error banner: 2404 passed, 0 failed; ESLint, TypeScript, Vite succeeded. Final log: `panel-verify-release.log`.
- Fixed enum labels after parent review: all FreeAI action stages and row failure categories localized, including diagnostic events carrying fixed reason codes.
- `bun test tests/registerbot.test.ts tests/localeParity.test.ts` after enum coverage: 17 passed, 0 failed.
- Final `bun run verify` after enum labels and regression coverage: 2405 passed, 0 failed; ESLint, TypeScript, Vite succeeded. Authoritative final log: `panel-verify-complete.log`.
- Targeted Prettier applied to new registerbot files, router/sidebar insertions and test portability changes.
- `git diff --check`: passed.

## Output

- `cpa-panel/dist/index.html`, single file, including the complete localized registerbot feature.
- 131 registerbot translation keys in each of all seven existing locales.
- No added dependencies; package and lockfile unchanged.
- Two pre-existing test files changed only for cross-platform path conversion and locale-independent exact currency assertions.
- Browser inspection, deployment and commits are owned by the parent task and were not executed by this subtask.

## Live validation follow-up

- Added the backend's new `network_error` stage label to all seven locales and the fixed-stage coverage test.
- `bun run verify` passed again: 2405 tests, 0 failures, 15961 assertions; ESLint, TypeScript and Vite succeeded.
- Latest authoritative log: `panel-verify-network-error.log`; latest single-file output: 4,096.32 kB (gzip 1,326.38 kB).
