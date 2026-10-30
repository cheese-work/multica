# CI warning regressions

The `warning-budget` CI job waits for build, test, lint and supporting jobs.
It reads their completed job logs and GitHub warning/notice annotations with
read-only permissions. It compares warning signatures and occurrence counts
against each job's latest successful execution in a successful `main` run of
the same workflow that predates the candidate run. The search includes pushes,
scheduled full runs and manual main runs, with pagination through up to 1000
successful main runs when newer runs skipped a job. Missing evidence beyond
that history bound fails closed. Pull-request runs cannot provide trusted baseline evidence.
The candidate cannot supply its own baseline. Missing main job evidence,
unavailable logs and incomplete candidate jobs fail the check.

Decision: the PR-only `migration-prefix-check`, `image-budget` and `go-lint`
jobs have a strict zero-warning baseline. The Go job's tool steps run only
on PRs, so its otherwise empty main executions are not meaningful baselines.
These jobs do not inherit allowances from another job or an earlier PR.

The report artifact records each job's baseline run URL and SHA in
`job_baselines`, including the original baseline job name. The runner pin
renames `cursor-background-lifecycle (ubuntu-latest)` and
`installer (ubuntu-latest)` to their `ubuntu-24.04` matrix names. Only these
two legacy names are aliases when selecting job evidence. Their warning
counts retain the candidate job name; no diagnostics are discarded.
The report records the strict-zero jobs in `zero_baseline_jobs`, warning counts,
regressions and the candidate's annotation list. `baseline_run` identifies the
newest trusted main run searched, not necessarily every job's baseline.
Existing warnings may decrease; a new
signature or an increased count fails. There is no suppression list or
candidate-editable allowance file. PID, ANSI, timestamp and BuildKit progress
noise are normalized. Test names and runner command-header metadata are not
diagnostics. Grouped output from the tools themselves is still counted.
Raw log downloads explicitly allow terminal escapes in `gh`; Node strips VT
control sequences before warning detection.
ESLint signatures use the repository-relative file and rule, not `line:col`
or line numbers embedded in the message. Counts still catch an additional
warning for the same file and rule. File-specific annotations retain their
path and match the corresponding lint log signature when available.
Every timestamped Go application `WARN` event matching
`YYYY/MM/DD HH:MM:SS[.fraction] WARN` is a runtime log event in every job,
not only in negative-path fixtures. These events are not counted;
all GitHub warning/notice annotations are still counted regardless of source.

Run the local regression fixtures with:

```sh
node --test scripts/warning-budget.test.mjs scripts/ci-warning-config.test.mjs
```

## CD warning sources

The original qualification/deploy evidence is in runs `40446860326` and
`40450314672`. On Node 24, `NODE_OPTIONS=--trace-deprecation` attributed
`DEP0040` to bundled `whatwg-url`/`tr46` in the old actions, `DEP0169` to
bundled `node-fetch` URL parsing, and `DEP0005` to the artifact downloader's
`MatcherStream` ZIP dependency. These are action bundles, not application
build/test dependencies. Upgrade the actions rather than disabling Node
deprecation reporting.

The repository uses major tags, not SHA pins. Releases checked for this change:

| Action | Current release | Workflow major | Runtime |
| --- | --- | --- | --- |
| actions/upload-artifact | v7.0.2 | v7 | node24 |
| actions/download-artifact | v8.0.2 | v8 | node24 |
| actions/setup-node | v7.1.0 | v7 | node24 |
| docker/login-action | v4.7.0 | v4 | node24 |
| docker/setup-buildx-action | v4.4.1 | v4 | node24 |
| actions/setup-go | v7.0.0 | v7 | node24 |
| docker/build-push-action | v7.4.0 | v7 | node24 |
| docker/metadata-action | v6.2.0 | v6 | node24 |
| pnpm/action-setup | v6.1.0 | replaced by pinned npm install | node24 |
| dorny/paths-filter | v4.0.3 | v4 | node24 |
| golangci/golangci-lint-action | v9.3.0 | v9 | node24 |
| goreleaser/goreleaser-action | v7.3.0 | v7 | node24 |
| azure/setup-helm | v5.0.1 | v5 | node24 |

The v6 pnpm action bootstraps pnpm 11 before switching to the repository's
pinned pnpm 10, producing a layout-migration warning. `scripts/setup-pnpm.sh`
instead installs exactly `package.json`'s pnpm version in `RUNNER_TEMP` and
exports only that package's bin directory. Invalid pins and failed installs
fail before publishing a PATH entry. No newer bootstrap or layout migration
is needed.

Each release's `action.yml` declares the recorded runtime. Ubuntu jobs pin
`ubuntu-24.04`. CD jobs do not install application dependencies and explicitly
disable automatic package-manager caching; frontend jobs explicitly cache pnpm.

Tailwind's optimizer used Lightning CSS 1.32.0, which warned about the valid
`::highlight()` pseudo-elements. The targeted `@tailwindcss/node>lightningcss`
override selects 1.33.0, whose release adds that support. Both find-highlight
rules remain intact. No CSS optimization or diagnostic is disabled.

The pnpm boxes listed lifecycle scripts without an explicit review decision.
The allowlist retains `esbuild`, `electron`, `msw` worker synchronization and
`unrs-resolver` native-loader preparation. `ignoredBuiltDependencies` explicitly
keeps the unnecessary `core-js` banner, `protobufjs` version check and
`unicode-animations` terminal demo disabled. It also disables
`electron-winstaller` preparation for the retired Windows build and `sharp`'s
install-time check; the lockfile supplies Sharp's platform-native packages.
These scripts do not execute. This is an audited lifecycle decision, not a
warning-log filter or blanket script approval.

The unencrypted-credential warning came from C00's remote cutover, not X99.
The shared registry login now uses an ephemeral environment-backed credential
helper. Success and failure cleanup restore the caller's Docker configuration
and remove the helper without persisting its token.

Buildx cleanup timed out while stopping this run's BuildKit container. Before
the action's post hook, an always-run step stops only that container with a
120-second grace period and removes only that builder with a 180-second status
budget. The step has a four-minute bound and fails rather than hiding cleanup
errors; the action still owns its context/certificate cleanup.
Decision: cleanup is intentionally fail-closed. A cleanup failure fails
qualification, so `cd-deploy.yml` does not start. A run that leaves its owned
builder behind is not qualified; cleanup errors are not downgraded to warnings.

The original item 6 red logs did not record an assertion and exit status.
Item 6 test-first red is NOT-RUN; passing credential fixtures are not red proof.

## Acceptance boundary

Local fixtures do not qualify a production deployment. Qualification remains
trusted-main-only and preserves provenance/SBOM requirements. A new CD
qualification after merge and an explicitly Cheese-approved C00 deploy must
still demonstrate zero items 1–7 using annotation lists and log grep. Do not
dispatch a deploy or change its approval environment to obtain that evidence.
