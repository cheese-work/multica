## What does this PR do?

**Status:** Draft. This PR syncs `cheese-work/multica` with upstream Multica release tag `v0.6.3`, following the release-tags-only decision (CHE-1054/CHE-1380). It is source integration only, not deployment approval. It stays draft until the dev team leader reconciles the independent different-lab review, OCR and current-head checks.

**Pinned refs:**

- Upstream tag `v0.6.3`: annotated tag object `61bf2402edf4a585f8356f9b68b03f9e1bdabafe`, peeled to commit `18fa9f728a9e25f2465df2789631edb2a75442ad`. Also upstream `main` at execution (17 Oct 2026, 08:40 ICT). No newer stable release existed then (`v0.6.3` is the highest `v0.6.*` tag).
- Fork base: `main` at `2b135399996d8072c5a99deb88ef55edb62f49b4` (after #290).
- Merge-base with `v0.6.2` (`7ab92d7622ecd81d179a8d83ae2dea74835a5069`). Divergence before the merge: fork `main` 240 commits ahead, 18 commits behind `v0.6.3`. No other sync PR or branch was open.
- Candidate commit: `1fea916f3` (single merge commit, no rebase). Tests-only first commit: none (merge-only candidate).

**Conditions at merge time:** A merge to `main` starts `cd-qualification.yml`, and its success triggers `cd-deploy.yml`, which deploys to C00 through the `c00-production` environment. The same push builds a new `multica-daemon-<os>-<arch>` artifact, and fork-built daemons auto-update to it (CHE-1580). Merging this PR is therefore a deployment-relevant decision.

## Related Issue

Closes CHE-1837

Parent: CHE-1835 stays open for review, OCR and the ready decision. This PR's title key closes only the author child CHE-1837.

## Changelog

Changelog: none

## Type of Change

- [x] New feature (upstream v0.6.3 sync: project workflows)
- [x] Bug fix (upstream v0.6.3 fixes)
- [x] Tests (adding or improving test coverage)
- [x] Documentation update

## Changes Made

All 18 upstream commits after `v0.6.2` merge in with a merge commit. Conflict decisions (17 conflicted files):

- **Onboarding shim routes** (`server/cmd/server/router.go`): drop. Upstream MUL-8047 deleted the handlers (`onboarding_shim.go`), so the routes cannot stay. Retain the fork's `RequireHumanActor` on `/api/cli-token` and `/api/auth/desktop-token`; upstream's plain `/api/cli-token` registration is not taken.
- **Migrations cleanup map** (`server/cmd/migrate/main.go`): add the three upstream `issue_workflow` index entries (`574`, `576`, `582`). Upstream's renumbered wakeup names are not added; the fork applies those under its own names.
- **Feature flags** (`featureflags/keys.go`): retain all fork keys (Jev, receipts, wakeup definition writes, export privacy); add upstream `ProjectWorkflowsV1`.
- **Run cost UI** (`execution-log-section.tsx`, `issue-runs-dialog.tsx`): adapt. Take the upstream redesign (cost bars, totals, per-agent chart). The fork's only delta there was the `costYPercent` guard (non-finite or zero `yMax`); it is reapplied in `issue-runs-cumulative-chart.tsx`, where `yPct` was still unguarded. `costYPercent` stays used by `stepCurvePath`.
- **Issue activity row** (`issue-detail.tsx`): adapt. Upstream's nested row with the workflow `TimelineHandoff`, plus the fork's wakeup wrapping, `WakeupActivityLinks` and test ids (CHE-1726).
- **Agent CLI** (`cmd_agent.go`): retain the fork's `runAgentEnvPatch`.
- **Comment-only conflicts** (`hermes_home.go`, `actor_guards.go`): take upstream's shorter comments; code is identical.
- **Builtin skill** (`multica-platform/references/issues.md`): retain the fork text and add upstream's project-workflows bullet.
- **Generated code**: `server/pkg/db/generated` regenerated with sqlc `v1.31.1`, not hand-merged.
- **Migration numbering**: upstream adds eight migrations (`573`–`579`, `581`, `582`) whose prefixes collide with fork migrations already applied. Nine stem pairs join `allowedForkCollisions` in `migrations_lint_test.go`, as in earlier syncs. No file is renamed. `TestMigrationNumericPrefixesAreUnique` passes. The fork's `capture_issue_wakeup` (latest definition: `532`) differs from upstream's `579` redefinition by one added predicate line, so the later-applied `579_wakeup_skip_handed_to_agent` is safe to apply on top.

Fork-only behavior checked and kept: runtime update routing (CHE-580/CHE-1580 paths untouched), workspace governance, comment/run-trigger protections, auth guards, chat/DM stack, wakeup scheduler. No fork-authored behavior change is added.

## How to Test

Test-First check: NOT-RUN. The PR is a merge-only sync with conflict resolutions; it adds no fork-authored behavior. The only fork-authored code lines are the `costYPercent` reapplication in `issue-runs-cumulative-chart.tsx` and the test allow-list entries. The existing `costYPercent` and `stepCurvePath` tests in `issue-run-timeline.test.ts` pass.

Gates run on X99 at head `1fea916f3` (tree of the merge commit):

1. `pnpm typecheck`: 10/10 tasks pass. `pnpm lint`: 7/7 tasks pass (0 errors, 22 views warnings from upstream code).
2. `go vet ./...` passes. `sqlc generate` (v1.31.1) leaves the tree unchanged.
3. Frontend tests, per package: docs 16, ui-lab 46, core 2372, desktop 678, web 337 pass. Views 6371 of 6375 pass in the full run; the 4 failures (`inbox-filter-menu`, `issue-detail` source-context, `table-view-editing`, `table-view-project-grouping`) are 5000 ms timeouts or a teardown `window is not defined` under load. The four files pass alone (152/152). A turbo-parallel `pnpm test` run also showed one timeout in web `text-contrast` and one in desktop `scripts/package.test.mjs`; both pass in the per-package rerun.
4. Migrations on an isolated `pgvector/pgvector:pg17` database: a database migrated with fork `main` (705 versions) upgrades to 714; the eight upstream migrations apply out of order (`573`–`578`, `581`, `582`; `579` was already recorded under the fork's name) and a rerun is a no-op. The upgraded and fresh 714-version databases match on column catalog, index definitions and function bodies (md5 of `pg_get_functiondef`).
5. Go suite (`scripts/go-test-with-agent-cli-guard.sh`; regular packages `-p 1`, `pkg/agent` `-p 2 -parallel 2`) against the isolated Postgres and Redis, from a clean worktree outside the agent task directory: all packages `ok`. One environment effect: `/tmp/.git` exists on X99, so `cursorProjectRoot` treated `/tmp` as a project and three `TestPrepareCursorMcpConfig*` tests collided on `/tmp/.cursor/mcp.json`. With `TMPDIR` outside `/tmp` the package passes. The fork code is unchanged by this PR.
6. `git diff --check` passes, except one trailing blank line in upstream `packages/views/workflows/templates.ts`.
7. CI: see PR checks (collected on CHE-1837).

Verification limits: Windows-only and macOS-only tests did not run; this fork has no Windows or macOS CI runner. `golangci-lint` is left to CI `go-lint`. No daemon was restarted and nothing was deployed. Rendered UI was not checked.

## Checklist

- [x] I have included a thinking path that traces from project context to this change
- [x] I have run tests locally and they pass (load-induced timeouts and one host `/tmp/.git` effect are recorded above)
- [ ] I have added or updated tests where applicable (merge-only; allow-list entries only)
- [ ] If this change affects the UI, I have included before/after screenshots (not applicable: upstream UI changes, screenshots NOT-RUN)
- [x] I have updated relevant documentation to reflect my changes
- [ ] If I added a new runtime / coding tool / UI tab, I synced the change to **landing copy** and **relevant docs** (not applicable)
- [ ] If this PR touches Chinese product copy, I checked it against `apps/docs/content/docs/developers/conventions.zh.mdx` (NOT-RUN: upstream copy taken as-is)
- [x] I have considered and documented any risks above
- [x] I will address all reviewer comments before requesting merge

## AI Disclosure

**AI tool used:** Multica Agent (Claude Code, x99-claude-sonnet)

**Prompt / approach:** Pin `v0.6.3`, merge it onto fork `main` with a merge commit, resolve each conflict by retaining or adapting fork behavior, regenerate sqlc output and run the gates on X99. The independent different-lab review and OCR are dispatched by the dev team coordinator after publication.
