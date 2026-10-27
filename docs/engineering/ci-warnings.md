# CI warning regressions

The `warning-budget` CI job waits for build, test, lint and supporting jobs.
It reads their completed job logs and GitHub warning/notice annotations with
read-only permissions. It compares warning signatures and occurrence counts
against the most recent successful `main` push of the same workflow that
predates the candidate run. The candidate cannot supply its own baseline.
Missing main evidence, unavailable logs and incomplete jobs fail the check.

The report artifact contains both run URLs, SHAs, warning counts, regressions
and the candidate's annotation list. Existing warnings may decrease; a new
signature or an increased count fails. There is no suppression list or
candidate-editable allowance file. PID, ANSI, timestamp and BuildKit progress
noise are normalized. Test names and command echoes are not diagnostics.
Raw log downloads explicitly allow terminal escapes in `gh`; Node strips VT
control sequences before warning detection.
Timestamped Go application `WARN` events emitted by negative-path fixtures
are runtime events, not compiler/test-harness diagnostics. They are not counted;
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
| pnpm/action-setup | v6.1.0 | v6 | node24 |
| dorny/paths-filter | v4.0.3 | v4 | node24 |
| golangci/golangci-lint-action | v9.3.0 | v9 | node24 |
| goreleaser/goreleaser-action | v7.3.0 | v7 | node24 |
| azure/setup-helm | v5.0.1 | v5 | node24 |

Each release's `action.yml` declares the recorded runtime. Ubuntu jobs pin
`ubuntu-24.04`. CD jobs do not install application dependencies and explicitly
disable automatic package-manager caching; frontend jobs explicitly cache pnpm.

Tailwind's optimizer used Lightning CSS 1.32.0, which warned about the valid
`::highlight()` pseudo-elements. The targeted `@tailwindcss/node>lightningcss`
override selects 1.33.0, whose release adds that support. Both find-highlight
rules remain intact. No CSS optimization or diagnostic is disabled.

The two pnpm boxes listed unreviewed lifecycle scripts. The allowlist now also
approves the lockfile's `core-js` banner, `msw` worker synchronization,
`protobufjs` version check, `unicode-animations` terminal demo and
`unrs-resolver` native-loader preparation. Script execution remains allowlisted;
there is no blanket approval, ignored-build list or log suppression.

The unencrypted-credential warning came from C00's remote cutover, not X99.
The shared registry login now uses an ephemeral environment-backed credential
helper. Success and failure cleanup restore the caller's Docker configuration
and remove the helper without persisting its token.

Buildx cleanup timed out while stopping this run's BuildKit container. Before
the action's post hook, an always-run step stops only that container with a
120-second grace period and removes only that builder with a 180-second status
budget. The step has a four-minute bound and fails rather than hiding cleanup
errors; the action still owns its context/certificate cleanup.

## Acceptance boundary

Local fixtures do not qualify a production deployment. Qualification remains
trusted-main-only and preserves provenance/SBOM requirements. A new CD
qualification after merge and an explicitly Cheese-approved C00 deploy must
still demonstrate zero items 1–7 using annotation lists and log grep. Do not
dispatch a deploy or change its approval environment to obtain that evidence.
