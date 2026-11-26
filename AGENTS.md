# Repository Instructions

This codebase is yours: what it's for and how it's built are your call, and you'll keep building on it.

Multica is a task management platform where people and agents collaborate on issues. These instructions apply to all coding agents working in this repository.

## Scope and Reading Order

- Before changing `apps/mobile/`, also read [apps/mobile/AGENTS.md](apps/mobile/AGENTS.md), even if your tool does not load nested instructions automatically. Platform-specific sections below apply only to the named platform.
- For naming, translations, or Chinese UI/docs copy, read [conventions.mdx](apps/docs/content/docs/developers/conventions.mdx) and [conventions.zh.mdx](apps/docs/content/docs/developers/conventions.zh.mdx).
- Maintain shared rules here and mobile-specific rules in the mobile file. `CLAUDE.md` files only import them. Update instructions in the same change that alters the referenced workflow or boundary; do not add incident timelines, dependency version lists, or duplicate rules.

## Sharing Rules and Package Boundaries

| Location | Responsibility and constraints |
| --- | --- |
| `server/` | Go backend; Chi, sqlc, WebSocket |
| `packages/core/` | Headless logic, API client, Query hooks, shared Zustand stores. No UI libraries, `react-dom`, `next/*`, or `process.env`. Client persistence goes through `StorageAdapter`; only the default adapter (`platform/storage.ts`) and two tab-scoped `sessionStorage` uses, exception dedupe (`analytics/exception-dedupe.ts`) and theme safe mode (`appearance/safe-mode.ts`), touch Web Storage. Cookies are only for values the server or Next.js proxy reads. Lint enforces the import, `process.env`, and Web Storage rules outside tests (`packages/core/eslint.config.mjs`). |
| `packages/ui/` | UI primitives and shared styles. No business logic or `@multica/core` imports. |
| `packages/views/` | Shared web/desktop pages and business components. No store definitions, `next/*`, or `react-router-dom`; use `NavigationAdapter`, `useNavigation()`, and `<AppLink>`. Lint enforces the store rule outside tests (`packages/views/eslint.config.mjs`). |
| `apps/web/` | Next.js routes/layouts and web-only UI. Framework APIs stay here; shared navigation adapters live in `apps/web/platform/`. |
| `apps/desktop/` | Electron and desktop-only UI/state. Application navigation goes through `apps/desktop/src/renderer/src/platform/`. |
| `apps/mobile/` | Independent Expo/React Native client: owns UI, state, hooks, providers, i18n, build, and release. Shares core types and pure utilities, including platform-independent schemas; runtime imports use the pure module's own subpath, not a barrel that also re-exports hooks or stores (lint-enforced). |
| `apps/docs/` | Fumadocs documentation site |

- Dependency direction is `views -> core + ui`; core and ui remain independent. Shared packages export raw TypeScript compiled by consuming apps.
- Extract logic used by both web and desktop into the appropriate shared package. Keep framework/Electron APIs in the app layer; inject platform-specific UI through props/slots.
- Wire shared features into both web routes and the desktop router or overlay. Reuse existing guards/providers such as `DashboardGuard` in `packages/views/layout/`.
- Each workspace declares its directly imported external dependencies. A dependency declared by two or more workspaces, or already in the `pnpm-workspace.yaml` catalog, uses `catalog:`; CI runs `scripts/check-dependency-catalog.mjs`, whose allowlist records known drift awaiting an upgrade. Mobile is exempt and pins Expo/React Native-aligned versions in its own manifest.
- This section is the source for the package-boundary table in `apps/docs/content/docs/developers/conventions*.mdx` (every locale); change them together.

## Settings-first features

The fork ships capability; Cheese Work configuration stays outside fork runtime code. Before you design a change, run three tests (`F0 Boundary` in the multica-dotfiles fork checklist): (1) identity: if it names a person, agent, host, lab, language or issue of one workspace, it is configuration; (2) second workspace: if a fresh Solo workspace would not turn it on, it is a plugin or guard rule, default off; (3) primitive: if a settings value, guard policy, workflow step text, wakeup rule or skill can express it, it is configuration and needs no fork PR. Only a change that passes all three goes into fork core, with a neutral default.

- Ship an opinionated default that works out of the box; every opinion is a setting. Gates use `off | warn | block`.
- Generalize the incident into a primitive: for example, catalog-based status entry conditions attached to upstream project-workflow columns, not a one-off "no draft PR in in_review" check.
- Provide presets such as Solo, Team with review, and Cheese strict so new workspaces are usable without tuning.
- Keep rules in settings, not prose. Agents read settings through the CLI; skills point to the setting.
- Keep fork cost low: use a generic settings store, not a new column or migration per feature; prefer shapes upstream can accept.

Each setting has a named default, a reason, and a scope (`workspace`, `project`, or `issue`). Review settings nobody changes for 30 days.

## Development and Verification

Use `Makefile`, workspace `package.json` files, and `pnpm-workspace.yaml` for current commands and versions. See [CONTRIBUTING.md](CONTRIBUTING.md) for setup and worktree operations.

- Use the checkout's managed environment: `make up`, `make status`, `make down`. `make down` preserves data; `make destroy` removes the environment and its data.
- Worktrees share PostgreSQL but have isolated databases/ports. Use the environment scripts and `.env.worktree`; do not copy the main checkout's `.env` or manually create a database through an assumed PostgreSQL instance.
- Regenerate sqlc with `make sqlc` after SQL changes.
- Run the narrowest useful checks while iterating, then broaden when risk warrants it. Report what actually ran and any skipped checks. Go: build a binary once with `go build -o` and reuse it, not repeated `go run`; locally run `go test ./<pkg>/... -run <Name>` and leave the full suite to CI unless the issue asks for a local full run.

Run these from the repository root:

| Scope | Checks |
| --- | --- |
| Frontend excluding mobile | `pnpm typecheck`, `pnpm lint`, `pnpm test` |
| `.github/` or `scripts/` | `pnpm test:scripts` (every `scripts/**/*.test.mjs`, including CI path selection and gates), plus `bash scripts/<name>.test.sh` for a shell script you changed |
| Go backend | `make test` (CI; locally use targeted `go test`, see above) |
| End-to-end | `pnpm exec playwright test` |
| Combined web/backend verification | `make check` |
| Mobile | Commands in [apps/mobile/AGENTS.md](apps/mobile/AGENTS.md#verification) |

Docs ship with the change. A PR that adds or changes user-visible behavior, a CLI flag, an env var, a GitHub App permission or event, or a self-host requirement updates the matching page under `apps/docs/content/docs/` in the same PR, in every locale (`.mdx`, `.fr`, `.ja`, `.ko`, `.zh`). A new App permission or webhook event also updates the **Self-hosting setup** section of `github-integration.mdx` (App permissions, subscribed events). A purely internal change skips docs and says why in the PR checklist. Docs-only changes can use link/reference checks and `git diff --check`; state that code tests were not run.

## State Rules

- TanStack Query owns API/server data. Zustand owns client state such as filters, drafts, modals, and tab layout; persist only durable preferences/drafts/layout, not server data or ephemeral UI state.
- Web/desktop shared stores live in `packages/core/`. Desktop platform stores remain in desktop; mobile stores remain in mobile. Do not define stores in `packages/views/`.
- Desktop tabs in a window share one set of Zustand stores and mount one at a time; a web tab gets its own. State describing how the current page was reached (source list, selection, highlight) must not assume one page: a click that opens a new tab leaves the current page's record intact and still records the target tab's own.
- On web/desktop, workspace identity is route-driven; platform mirrors exist only for request headers, storage namespaces, and reconnects. React Context is for platform plumbing, not a second server-state store.
- Among stores, only auth/workspace stores may call `api.*` directly; other server interactions belong in queries/mutations.
- Workspace-scoped query keys include `wsId`; account-level keys remain account-scoped. Hooks needing workspace context accept `wsId` unless guaranteed to run under its provider.
- Zustand selectors return stable references; use shallow comparison for allocated objects/arrays.
- WebSocket events patch or invalidate Query caches, not server payloads in Zustand. Clearing client-owned pointers is allowed with one responder and a self-initiated guard when this client can cause the event.
- Optimistic field patches require a predictable result, rare failure, trivial rollback, and staying on the current screen. Snapshot before patching, roll back on failure, and invalidate uncertain projections on settle.
- Create/delete/leave and confirmation flows await the server before navigation or cleanup; do not optimistically delete entities. Exceptions: workspace leave (known debt under Desktop Rules) and mobile inbox mark-read (mobile instructions).
- When the caller can unmount before the request settles (moving to the next item, closing a dialog or menu), attach toasts and follow-up work to the request promise (`mutateAsync().then(onSuccess, onError)`) or to `useMutation`'s own callbacks, not to per-call `mutate(vars, { onSuccess, onError })` callbacks, which React Query skips once the caller unmounts. Test these flows with the real mutation hook and a mocked API, unmounting the caller before the request settles (`packages/views/inbox/components/use-inbox-snooze.test.tsx`, `packages/views/issues/actions/__tests__/issue-inbox-actions.test.tsx`).
- Message sends use visible pending state and retry on failure.
- Authored input (drafts, notes, choices) persists through the keyed draft store (`packages/core/drafts/`) until a confirmed send or explicit discard; component state alone is not enough in virtualized lists, dialogs or routes.

## API Compatibility

Installed desktop clients may talk to newer backends. Preserve response compatibility at the API boundary.

- UI-consumed JSON passes through a zod schema and `parseWithFallback`, not an `as T` cast. Web/desktop use `packages/core/api/schema.ts`; mobile uses its own request helpers.
- Provide defaults for optional fields and fallbacks for unknown server enums. Prefer explicit boolean checks; avoid tying critical affordances to a single backend flag when other contract signals are available.
- When adding/changing an endpoint, update its schema and malformed-response tests.

## Database and Migration Rules

- Do not add foreign keys, cascading deletes, or cascading updates. Validate relationships and clean up dependents in application code, using a transaction when the operation must be atomic.
- Every migration-created index, including indexes on new tables, uses `CREATE [UNIQUE] INDEX CONCURRENTLY`. Each concurrent index build gets its own single-statement migration file; the runner executes files outside an explicit transaction.
- Register every concurrent index build in `server/cmd/migrate/main.go` so an interrupted build's INVALID index is dropped before retry: up-direction builds in `concurrentIndexCleanups`, down-direction rebuilds in `concurrentDownIndexCleanups` (version → index name).
- Before pushing a migration, rebase onto the latest `origin/main`, number it as the current highest prefix plus one, and run `go test ./cmd/migrate ./internal/migrations` from `server/`.
- A PR's green CI covers only the main it ran against; main can take the same number later without a textual conflict. Right before merging a PR that adds migrations, run `git fetch origin main` and `node scripts/check-migration-numbers.mjs --head <PR head>`. A failing check, or a failing `Migration numbers vs latest main` status on the PR, means renumber first, never merge as is.
- Conditionally skipped migrations are still recorded in `schema_migrations`. Later DDL touching conditional objects must be idempotent (`IF EXISTS` / `IF NOT EXISTS`); document recovery if the missing object would break runtime behavior.

## Row Lock Order

Task writes and runtime-side paths lock the same `agent`, `agent_runtime`, `workspace`, and `issue` rows; two paths taking a pair in opposite orders with conflicting modes deadlock (MUL-9501).

- Task enqueue order: the `agent` row first, then `lock_task_owner_rows` (migration 284) inside the writing statement takes FOR KEY SHARE on the workspace, issue, and runtime rows. New enqueue paths keep this order.
- A path that locks `agent_runtime` before `agent` must let that FOR KEY SHARE through. To exclude only UPDATE/DELETE of the runtime, use FOR NO KEY UPDATE (`LockAgentRuntimeForClaimDelivery`, `LockAgentRuntimeForAccessChange`). Use FOR UPDATE (`LockAgentRuntime`) only when new rows must be stopped from referencing the runtime: runtime delete and teardown.
- A new path that takes these rows in the reverse order ships with a test that forces the interleaving, as `server/internal/handler/chat_send_claim_deadlock_test.go` does.
- Known, unfixed: runtime delete and teardown (`TeardownRuntime`, the runtime delete handlers, the runtime GC sweeper) take `LockAgentRuntime`, then the agents via `ListUserAgentsByRuntimeForUpdate` — the reverse of enqueue order. It can only deadlock with an enqueue while that runtime is being deleted.

## Backend UUID Rules

In `server/internal/handler/`, distinguish UUID sources before using them in writes:

- UUID-or-human-readable resource params: resolve with loaders such as `loadIssueForUser`, `loadSkillForUser`, `loadAgentForUser`, or `requireDaemonRuntimeAccess`, then write using the resolved `entity.ID`.
- Pure UUID request input: `parseUUIDOrBadRequest(w, s, fieldName)`; return immediately when `ok=false`.
- Trusted sqlc/test-fixture round-trips: `parseUUID(s)`, which panics on invalid input.
- Outside handlers: `util.ParseUUID(s)` and check the error.

Workspace-scoped queries filter by `workspace_id`; membership gates access and `X-Workspace-ID` selects the workspace. Assignees are polymorphic: interpret `assignee_id` together with `assignee_type`.

## Desktop Rules

- Workspace session routes are tab destinations. Pre-workspace one-shot flows (create workspace, accept invite) use `WindowOverlay` in `apps/desktop/src/renderer/src/stores/window-overlay-store.ts`, not new routes. Stale workspace tabs heal by dropping stale tab groups.
- Workspace route layouts own `setCurrentWorkspace(slug, uuid)` from `@multica/core/platform`; leaving workspace context calls `setCurrentWorkspace(null, null)`.
- Cross-workspace navigation uses the adapter's `switchWorkspace(slug, targetPath)` flow; do not bypass it with direct router navigation.
- Workspace leave clears/navigates first to avoid the `member:removed` race; this is known debt in `packages/views/settings/components/workspace-tab.tsx`, not a pattern for new flows.
- Full-window views outside the dashboard shell mount `<DragStrip />` from `@multica/views/platform` as the first flex child. Interactive controls in the top 48px need `WebkitAppRegion: "no-drag"`.

## UI Copy

- Descriptions are optional and omitted by default. Do not restate titles, labels, values, statuses, or button actions; add help only for a non-obvious choice, constraint, consequence, or next step. A description prop is not a requirement to write a paragraph.
- Before adding or reviewing copy, follow "UI descriptions" in [conventions.mdx](apps/docs/content/docs/developers/conventions.mdx), and review all supported translations, including mobile's independent copy.

## Web/Desktop UI Rules

- For Button and Dialog usage, read `packages/ui/docs/button.md` and `packages/ui/docs/dialog.md`.
- Before building or reviewing an interaction that changes layout (overlays, expand/collapse, list rows, drag and drop, content switches, selection indicators, value feedback), read `packages/ui/docs/motion.md`: tokens, reduced motion, where not to animate, and the shared primitives. Such a PR attaches a GIF and the frame-sampling results from `scripts/motion/capture.mjs`.
- Prefer existing shadcn/Base UI primitives. Add components with `pnpm ui:add <component>`.
- For `pnpm ui:add @reui/<name>`, decline overwrite prompts. Keep `REUI_LICENSE_KEY` in the environment, never in repo files. Adapt vendored primitives into `packages/ui/components/ui/` and compositions into `packages/views/`.
- Import icons from `@multica/ui/icons`; no icon library is installed. If the icon you need is missing, draw it by following `.agents/skills/multica-icons/SKILL.md`. Do not substitute an inline SVG or an emoji. Rewrite icon imports in `ui:add` output before committing. Exception: proportional-width optical keycap glyphs live exclusively in `KBD_GLYPHS` inside `packages/ui/components/ui/kbd.tsx` and are only for `<Kbd>`.
- Use shared semantic tokens in `packages/ui/styles/`. Typography uses the role-named `--text-*` scale in `packages/ui/styles/tokens.css`, not Tailwind's default size ramp.
- Selected states remain identifiable on hover. Handle overflow, long text, and scrolling deliberately; avoid unnecessary local state and dividers.
- Issue links use the identifier (`MUL-123`): `paths.issueDetail` takes an `IssueRouteRef` from `issueRouteRef(issue)`. With only a UUID, use `useIssueRouteRef` (or `cachedIssueRouteRef` in event handlers). A UUID URL is rewritten to the identifier after load, which mounts the issue page a second time on web.

## Daemon Tool Shims

- The daemon puts a guard `bin/` first on `PATH` for every run, on every runtime, with a `git`, a `gh` and a `sleep` shim (`server/internal/daemon/guard_shims.go`, CHE-5194). It does not depend on credential-broker enforcement.
- A shim execs the first `git`/`gh` on the `PATH` it fronts. Install credential-aware shims (`broker.InstallShims`) on `agentEnv["PATH"]` before calling `frontGuardShims`, so the guard hands over to them and never bypasses the broker. Custom env cannot override `PATH`. The `git`, `gh` and `sleep` shims run `multica guard exec` (the daemon’s own binary). Each `MULTICA_GUARD_REAL_<TOOL>` points to the next credential-aware tool, including cross-tool lookups.
- Plugin 2 (`ci-wait`, CHE-5206) refuses `gh pr checks --watch`, `gh run watch` and `sleep` of 20 s or more on every runtime, with the wakeup line, and `gh api graphql` mutations. `gh api` writes to `repos/*/git` stay refused except create and delete of `refs/heads/red-main/<key>` and `refs/heads/red-main-owner/<key>/<claimant>` (`scripts/main-health/red_main_claim.py`, CHE-7519). The Claude hook (CHE-1965) and the Codex hook (CHE-5176) stay until that runtime has a tested blocking prevention; a PR check does not count for CI-wait.
- The guard policy is `workspace.settings.guard_policy` (CHE-5204): workspace plugins and review rules plus `layers` (org, repo, path; `locked` freezes keys against deeper layers). `guard.ParsePolicy` is the one validator for the generic save, the decision-action patch, the daemon and `multica guard plugin set`. The daemon writes the policy to a 0600 file in the task temp dir and sets `MULTICA_GUARD_POLICY` + `_SHA256`. `Engine.Exec` resolves the layers for each call's target (`-R`, else the repo of a `gh pr <verb> <PR URL>`, else `gh repo view`); the directory (`git rev-parse --show-prefix`) is read only when a path layer names that repo, and then only inside a clone of it. It refuses the call when a layered policy cannot learn the repo or a required directory. A lock on a review pattern covers every pattern it matches (`*`, `owner/*`). A policy that is stored but undeliverable or unverifiable fails closed: the guard refuses every `git` and `gh` call for the run, because the stored rules are unknown. The key is on the decision-action allow table, so agents propose changes with `multica guard plugin set`.
- `repository_allowlist` in the workspace policy restricts exact repositories per owner before any plugin: checkout, Git push, GitHub PR create/comment. Omitted owners are unrestricted; listed owners default to deny, case-insensitively. Git resolves effective push URLs with the same global options. Unresolved destinations refuse. Deeper layers cannot weaken this boundary. Checkout reads token-bound workspace settings before touching the cache. Approving a policy change requires the existing passkey action after compatible daemon deployment.
- A repository states its own PR rules in `.github/agent-rules.yml` (CHE-5205; `guard.ParseRepoRules`): `version: 1`, `title` (`pattern`, `require`, `forbid`), `trailer` (a key such as `Refs:`), `reviewers`. It is schema-checked data, never code (unknown keys, a second document or more than 64 KiB are errors). The guard reads it through the GitHub API at the current tip of the PR's protected base branch, in the PR's target repository (never a fork, the PR branch or the working tree), and logs the tip SHA with the decision. It re-reads on every `gh pr create`, `gh pr ready` and `multica guard check-pr` (the CI entry point; read-only, exit 1 on a broken rule), so a new base tip or a changed review policy applies at the next call. Missing file: no repo rules. Unprotected base: file ignored, logged. Malformed file: create and ready refuse naming the file; unreadable base or file (API error): refuse and retry. The file is the lowest layer: any policy layer overrides it key by key (pattern, trailer, each title token) and a locked layer's key beats it; policy layers carry the same `rules` object. File `reviewers` add to the review rule's and never apply when it says `skip`.
- The blocking backstop is the `PR guard rules` check (CHE-5208): reusable `pr-guard-rules.yml`, called from `pr-guards.yml` (every PR push), `pr-guard-rules-edit.yml` (a title, body or base edit, a removed reviewer; a separate workflow so the CHE-4385 run budget holds) and `pr-guard-rules-queue.yml` (every merge-queue build, so the PR is judged again at merge time). It builds the CLI from the trusted commit (the queue's base commit on a merge-queue build, since the queue ref holds PR code) and runs `multica guard check-pr` (`scripts/pr-guard-rules.sh`), so a PR opened around the shim (absolute `gh` path, web UI) fails on a missing `[merge]`/`[squash]` tag, missing policy reviewers or a broken repository rule. Read-only permissions, no secret. A stored policy reaches it through the repository variables `GUARD_POLICY_JSON` (the policy JSON that `multica guard plugin set --file` takes) and `GUARD_POLICY_SHA256` (its sha256). Both unset (no stored policy, CHE-5934): the CLI judges on the compiled default policy, as the daemon's guard does, with a notice. Only one set, or a digest mismatch: the check is red. The variables are a copy of the stored policy, so update both when the stored policy changes. A new base-branch rule, a changed variable or a stale earlier green is caught by the merge-queue build; a copy that nobody updated is caught only when the read-only secret `GUARD_POLICY_READ_TOKEN` is set (CHE-5752): the check then reads the live `settings.guard_policy` (`vars.GUARD_POLICY_SERVER_URL`, `vars.GUARD_POLICY_WORKSPACE_ID`) and fails closed (exit 2) on a differing copy or an unreadable live policy; `vars.GUARD_POLICY_LIVE_REQUIRED=1` makes a missing token red too. Until the secret exists, whoever changes the stored policy updates both variables in the same change. Making the check required is a branch-protection setting.
- The decision store (CHE-5207): the daemon sets `MULTICA_GUARD_LOG` to a file in the task temp dir (the guard appends one JSON line per decision) and uploads the first 500 lines to `POST /api/daemon/tasks/{taskId}/guard-decisions` when `runTask` ends. The server keeps them in `guard_decision`, idempotent on `(task_id, seq)`. `GET /api/guard/decisions` and `GET /api/guard/stats` (per plugin, per day in the viewer's time zone) feed both `multica guard log|stats` and the Guard settings page, so the numbers match. A run that dies before the upload loses its log.

## Testing

- Tests live beside their implementation: shared logic in core, shared components in views, platform wiring in apps, E2E in `e2e/`, Go tests in server. Do not test shared behavior in app suites.
- Give each behavior one canonical test layer: helper tests own parsing/state matrices; component tests cover wiring, accessibility, happy paths, and named regressions. Prefer a failing regression test before behavioral fixes.
- DOM-free `.test.ts` files start with `// @vitest-environment node`; do not use it if it would silently switch the code under test to an SSR path.
- Every workspace with jsdom suites loads `@multica/vitest-setup/web-storage` in its Vitest setup; do not add per-package `localStorage` shims.
- A test that reads files outside its package and that package's workspace dependencies declares them as `$TURBO_ROOT$/...` in the package's `turbo.json` `tasks.test.inputs`, and in the CI path group that runs it (`.github/ci-paths.json`); otherwise CI replays a cached pass when only those files change. `scripts/check-test-cache-inputs.mjs` enforces this.
- Views tests must not mock `next/*` or `react-router-dom`. Mock stores with their Zustand callable shape plus `getState`; mock API calls at `@multica/core/api`.
- E2E setup/teardown uses `TestApiClient`.
- DB-backed Go tests use `server/internal/testutil` fixtures (`dbfx.Issue`, `dbfx.Task`, `dbfx.Insert`) and `testutil.Call(h, req).Want(status).JSON(&out)`. Keep product assertions and case-specific diagnostics in the test, not fixture helpers.
- DB-backed Go tests share one database with packages running in parallel. When code under test reads across workspaces (sweeps, outbox scans, GC), assert on your fixture's rows (task state, `pending` / `settled` for your IDs), never on the call's totals or result length.
- Tests of CI configuration assert invariants a gate cannot see on its own, such as no `continue-on-error` in gated jobs, or behavior derived from the configuration, such as path selection and gate mapping. Do not assert the current text of a step, name, or matrix.
- Default tests must not resolve or execute user-installed agent CLIs; pass test-created fake or missing executable paths. New default agent commands go in `scripts/agent-cli-command-names.txt`.
- Only run real-agent smoke tests when explicitly authorized. Gate them behind `agentintegration` and check `MULTICA_RUN_REAL_AGENT_SMOKE=1` before executable lookup/account access. Run the specific test: `(cd server && MULTICA_RUN_REAL_AGENT_SMOKE=1 go test -tags=agentintegration ./pkg/agent -run '<test-name>' -count=1 -v)`.

## Change and Delivery Rules

- Keep changes scoped; reuse existing patterns.
- Before adding complexity for a speculative or vanishingly unlikely race or security edge case, explain the concrete failure mode, likelihood, and complexity cost to the user and get their buy-in. Do not silently expand scope for theoretical robustness.
- Do not add internal compatibility shims, dual writes, fallback paths, or legacy adapters unless requested. This does not relax API response compatibility above.
- Use names, types, and tests to express behavior. Code comments are English and kept for non-obvious rationale and constraints; remove superseded plans and incident narratives when updating code. Preserve tool directives and mixed-version contracts.
- When a change alters user-visible behavior, a setting, a default, a shortcut, or an entry point, `rg` `apps/docs/content/docs` for the feature's name and the terms of the old behavior; a newly added label is not in the docs yet. In the same PR, update every affected page and its locale siblings (`inbox.mdx`, `inbox.zh.mdx`, ...). Text the change makes wrong is always fixed here; if the docs only lack the new behavior, you may link a docs follow-up issue from the PR description instead. Exception: do not document features gated behind a default-off flag in `server/internal/featureflags/keys.go` (permanently enabled compat keys do not count) in `apps/docs/content/docs/` or root `README*.md`; note why docs were skipped in the PR description instead. When a PR flips a flag to default-on or removes it for general availability, add the docs across all locales in the same PR (content removed earlier can be recovered from the diff of PR #11111).
- A new built-in runtime prefix (`MULTICA_<PREFIX>_PATH`/`_ENDPOINT`) or `MULTICA_<PREFIX>_ARGS` variable must appear in every `apps/docs/content/docs/environment-variables*.mdx`; `apps/docs/lib/env-var-coverage.test.ts` enforces this, and its exemption list is for runtimes deliberately kept out of the docs.
- New global pre-workspace routes use a single word or `/{noun}/{verb}`, not hyphenated root names. Update `server/internal/handler/reserved_slugs.json`, run `pnpm generate:reserved-slugs`, and commit `packages/core/paths/reserved-slugs.ts` when changing reserved slugs.
- Use atomic conventional commits and the repository PR template. For releases, follow [.github/RELEASING.md](.github/RELEASING.md); default to a patch bump unless specified otherwise.
- Every PR description carries one `Changelog:` line outside HTML comments and code fences: one user-facing English sentence (`Changelog: Issue lists now open instantly in large workspaces.`), `Changelog: none`, or a section override (`Changelog(fix): Comments that end in an emoji no longer lose their last word.`). Not plumbing: `Changelog: Refactor X store`, `Changelog: CHE-123 add Y`. Keep the line verbatim, like `Closes`/`Refs`, including in PR-format rewrites. A PR without a valid line is hidden from `/changelog`. The PR that lands a stack on `main` carries the line for the whole stack. `Changelog: none` is only for internal-only changes: a feat, fix or perf PR that changes user-facing code fails the check with it. Upstream-sync PRs use `Changelog: none`. Details: [.github/PULL_REQUEST_TEMPLATE.md](.github/PULL_REQUEST_TEMPLATE.md).
- A PR that touches CLI commands (`server/cmd/multica/`), the agent brief or the daemon (`server/internal/daemon/`) also carries `Agent-facing: yes | no` and, on yes, `Adoption item:` (what agents must change). Once a day, after C00 runs the deployed revision, `scripts/agent-adoption.mjs daily` opens one adoption issue listing the yes PRs merged since the last one (feature, what agents change, owner) and the per-host CLI readback for new flags; no yes PR, no issue (CHE-5717).
