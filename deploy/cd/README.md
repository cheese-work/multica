# D1 image qualification, D2 migration quiescence controller

This directory contains the build-side (D1) and deployment-time (D2) halves
of CHE-372's deployment design. D1 builds a linux/amd64 backend and web image
pair from a trusted `main` push, records their immutable registry digests in
a release manifest, and validates the tuple before a later controller can
consider it for deployment. D2 is the cutover controller that actually
migrates the database and switches traffic; it is the only component
authorized to mutate C00.

Neither D1 nor D2 as implemented here contact C00, receive C00 deployment
credentials, receive recovery private keys, or use restored production data.
D4 (full C00 rehearsal, not yet implemented) is what actually activates
against C00, gated on its own separate approval.

## Persistent D1 build cache

`cd-qualification.yml` builds backend and web in a two-entry X99 matrix.
Each image gets its own runner/DinD budget, rather than sharing one DinD's
4 GiB and 3 CPUs. The D1 fixture suites run once in the hosted `d1-fixtures`
job, and the final `cd-qualification` job also runs on a hosted runner, so
neither takes an X99 queue slot (CHE-3983). `cd-qualification` keeps the
existing check name and assembles the same `cd-build-evidence-<sha>`
artifact after both images and `d1-fixtures` succeed. Images can push before
the fixtures finish, but no evidence manifest exists unless they pass. The web job supplies the exact optional fork entries used
in its image, so manifest assembly cannot hash a different download.

Each runner's Docker daemon retains the named `multica-cd-backend-v1` and
`multica-cd-web-v1` builders. They use `docker-container` for provenance and
SBOM generation. A bounded stop releases builder memory after the job;
the builder and its cache volume remain for later trusted main pushes.
BuildKit GC targets at most 4 GB per builder, reserves 1 GB, and targets
10 GB free. Each builder admits at most two build vertices at once.
No registry cache or untrusted PR cache is imported.

Go compilation uses `/root/.cache/go-build` cache mounts, including the
four daemon targets. Unversioned utilities compile before the release
arguments; server and daemon linking still consume the exact release
version and commit. Web builds retain `/app/apps/web/.next/cache`. The web
`builder` stage extends `deps` instead of copying `node_modules` into a second
layer, and sets `NEXT_SKIP_TYPECHECK=true` because `pnpm typecheck` already
gates every PR in `ci.yml`.
`NEXT_PUBLIC_*` stays after dependency installation but before compilation:
Next.js embeds those values in client bundles, so moving them after the
build would ship stale release metadata.

## D2: quiescence and supervised migration

`quiescence.mjs` implements the two admission gates from the approved D2
design (`CHE-372` addendum, reviewed digest
`cb44055dce9e3c587c93cb7e777a0f635e82d80bd3fde24397fef163b88d1873`):

- `preflight` — coarse, cheap check while the healthy release still serves.
  Denies on anything already known to be a hard blocker (an old transaction,
  a prepared transaction, a foreign advisory-lock holder, an unknown backend
  type, a concurrent index build already in progress) so the caller never
  starts an outage to wait out a problem it can already see. A young
  transaction is not itself a blocker here — only a note that quiescence
  still needs to be reached.
- `final-gate` — the strict, zero-tolerance check run immediately before the
  first candidate mutation and again immediately before migrator launch.
  Any open transaction, retained snapshot, prepared transaction, foreign
  advisory lock, unaccounted lock, or unfenced foreign session denies.
  Unknown state (a failed observation) always denies — it is never treated
  as an empty/healthy result.

Self-exclusion (never seeing the observer's own current query as a foreign
session) is handled entirely server-side, via the observation SQL's own
`pid <> pg_backend_pid()` clause. No client-controlled field —
`application_name` in particular — is ever used to exclude a row from either
gate. An earlier version of this module excluded rows by a fixed
`application_name` prefix, which was a real admission hole: `application_name`
is a session GUC any client can set to an arbitrary string, so a foreign
session could claim that prefix and hide an open transaction from the final
gate. `test-quiescence.sh` and `test-migrate-supervised.sh` both include a
negative control that spoofs the old prefix and asserts it is still denied.

### Timeout enforcement: Go-level, not an external probe

Timeout proof does not attempt to read back another live backend's
session-level GUC value from outside — PostgreSQL provides no view for that
(`pg_stat_activity` carries no such column, and `pg_settings`/
`current_setting()` only ever report the caller's own session), and a
client-supplied connection-string `options=` parameter is applied by pgx
*above* both PGOPTIONS and any role/database default (verified against
pgx v5's `pgconn/config.go` precedence and PostgreSQL's own GUC precedence
order), so any client-side setting — PGOPTIONS or a role/database default —
can be silently defeated by the connection string itself.

Enforcement instead lives inside the migrator's own Go process:
`server/internal/dbstartup.NewPoolWithEnforcedTimeouts` installs a pgx
`AfterConnect` hook that runs a `SET` on every physical connection the pool
opens — strictly *after* pgx has finished applying the connection string —
covering the pinned advisory-lock connection and every separately-opened
hook connection alike, then reads the values back on that same connection
and rejects the connection outright if they don't match exactly (including
rendering as 0, Postgres's "disabled" sentinel). Being the *last* setting
applied is what makes this immune to a connection-string bypass a
role/database default could not resist. `server/cmd/migrate` opts into this
via `MULTICA_INTERNAL_D2_ENFORCED_STATEMENT_TIMEOUT_MS`/
`MULTICA_INTERNAL_D2_ENFORCED_LOCK_TIMEOUT_MS` (unset by default — zero
effect on non-CD migrator invocations); `migrate-supervised.sh` sets both
before launching the migrator.

`find-live-session-by-role`/`verify-live-session-is-covered` in
`quiescence.mjs` exist for a different purpose: after launch, the watchdog
needs to locate the migrator's actual Postgres backend (PID + `backend_start`,
not the OS-level child process ID) to confirm *server-side* absence during
cancellation — they never attempt to read or verify a GUC value.

Every observation query is a fresh, autocommit `psql` invocation (no
persistent connection, no long-lived snapshot from the observer itself),
bounded by both a server-side `statement_timeout` and a wall-clock `timeout`
wrapper. On a host without `psql` on `PATH` (the case on X99 CD runners),
pass `--psql-via-docker-network <network>` to run each observation through a
short-lived `postgres:16-alpine` container on the same Docker network as the
target, mirroring D1's existing synthetic-fixture convention.

### The supervised migration runner

`migrate-supervised.sh` wraps `server/cmd/migrate` under **one absolute
deadline that governs the entire sequence** — settings enforcement, launch,
live-session discovery, the run itself, and any cancellation/confirmation —
computed as `migration_deadline = min(migration_started+15s,
cutover_started+40s)`, `work_deadline = migration_deadline - 2s`. There is no
separate, unbounded sub-phase (an earlier version had a 25-attempt discovery
loop with its own budget before the watchdog even started, which could
consume the whole allocation before the deadline was ever checked).

On cancellation, the watchdog requires **both**: the OS process confirmed
exited (not merely signalled), *and* the migrator's identified Postgres
backend confirmed absent — a client that has exited does not prove the
server-side session has finished rolling back or releasing locks,
especially mid-statement. Server-side absence is proven by
`quiescence.mjs`'s `verify-live-session-is-absent`, which checks the
*exact* recorded `(pid, backend_start)` pair against a fresh
`pg_stat_activity` read and also accounts for any *other* live session
still authenticated as the same role/database (the migrator's own hook
connections, or an ambiguous same-role session) — not a `LIMIT 1` query
compared loosely against a remembered PID, which can be satisfied by an
unrelated session while the real one is still live. An observation
*failure* (connection error, timeout) is UNKNOWN state and is never
treated as proof of absence; a migrator session that was never positively
identified can also never count as "confirmed absent" — both are
fail-closed, not a special case skipped by an `if` guard. A
deadline-exceeded or unconfirmed run always exits `3` (`needs_operator`)
and never claims success; it does not attempt to repair or roll back the
database itself.

The absolute deadline covers cancellation too, and is a **hard boundary on
waiting/confirming**: the cancel-confirmation loop, the SIGKILL-confirmation
loop, and the final absence probe are all bounded by the same
`deadline_epoch` computed once at the top of the script, never by a fresh
`now + reserve_seconds` clock started at cancellation time (which would
silently re-grant the reserve every time cancellation itself took any time
to notice the deadline had passed) — and none of them may even *start*
once the deadline has passed; reaching the deadline with anything
unconfirmed goes straight to the deterministic `needs_operator` report
rather than attempting more work there is no time budget left for.
Sending the escalation SIGKILL itself is the one exception and is never
deadline-gated: it is a single, effectively instantaneous syscall, not
wall-clock work, and it is also the last safety action available — skipping
it because the clock already reads `deadline_epoch` would leave a
still-running process with nothing further attempting to stop it, which is
strictly worse than a late confirmation of that same kill.

**Gating on start is not enough — the clamp must cover the entire
observer invocation, including launcher overhead, not just the SQL/timeout
logic that runs after the tool has already started.** `quiescence.mjs`'s
own internal `timeout -s KILL` wrapper (inside `runPsql`) is only installed
after `node` has already started, loaded its modules, and parsed `argv` —
none of that is free, and none of it was covered by any clamp on its own.
`migrate-supervised.sh`'s `run_node_bounded` closes this: it computes exact
remaining milliseconds against the relevant deadline immediately before
launching, denies outright without spawning anything below
`MIN_OBSERVER_BUDGET_MS`, and wraps the **entire `node ...` invocation** —
Node's own startup and CLI parsing included — in an external
`timeout -s KILL <exact-remaining-seconds>s`. `quiescence.mjs` still
receives its own, slightly smaller `--query-timeout-ms` so its internal
timeout fires first under normal conditions with a clean, attributable
error message; the outer `run_node_bounded` wrapper is the true backstop
that also catches anomalously slow Node startup or a script that never
reaches its own internal timeout logic at all (verified directly: a
`node -e 'setInterval(() => {}, 1000)'` that never installs any timeout of
its own is still killed by the outer wrapper at the requested boundary).
`MIN_OBSERVER_BUDGET_MS` is calibrated to real `--psql-via-docker-network`
overhead (`docker run --rm postgres:16-alpine psql ...` measured ~450ms on
X99 with a warm image cache, independent of query complexity), not an
arbitrary small number — a floor below that makes success structurally
implausible, so attempting the call anyway would burn the last of the
remaining time on something that was never going to finish.

Every fixed-interval `sleep` in the watchdog and cancellation loops
(`sleep_clamped_to`) is likewise capped at the actual remaining time
before its own deadline, never a bare constant — a `sleep 0.2` at the
bottom of a loop that ignores how close the deadline already is can carry
that loop past the deadline before it is ever re-checked, on its own,
independent of any observer call.

`quiescence.mjs`'s `timeout` wrapper itself sends `SIGKILL` (`-s KILL`)
rather than the default `SIGTERM`: against the `--psql-via-docker-network`
path specifically, `SIGTERM` lets the `docker` CLI attempt a graceful
container stop/detach with the daemon, measured taking over 2 seconds
against an unreachable target — more than 40x the requested budget.
`SIGKILL` ends the wrapped process immediately; this observer never needs
graceful shutdown semantics, only a hard ceiling on its own wall-clock
footprint.

A nonzero migrator exit is captured explicitly (`set +e` / `set -e` bracket
the one `wait` call that reads it) rather than being read via a bare `wait`
under the script's own `set -e`, which would otherwise abort the wrapper
script itself on the child's exit code before any of its own success/
failure/needs_operator logic ever ran.

### The CD entrypoint and its Compose wiring

`docker/entrypoint.cd.sh` is a new, explicit deployment-mode entrypoint that
gates `exec ./server` behind an external decision file the D2 controller
writes, instead of the stock `entrypoint.sh` unconditional migrate-then-serve
chain. The decision file's second line must be an attempt id matching
`CHE372_D2_ATTEMPT_ID` exactly — this is a freshness check: a decision file
left over from an earlier attempt must never be readable as current just
because a file with `starting_candidate` on its first line happens to exist
at that path. `entrypoint.sh` itself is unchanged and remains the default for
non-CD use; the `Dockerfile` now copies both entrypoints into the image, and
`deploy/cd/d2-controller.compose.yml` is the actual Compose override that
exercises `entrypoint.cd.sh` against a real built image —
`test-entrypoint-compose.sh` is the integration receipt proving the override
genuinely takes effect rather than being merely written and unit-tested in
isolation.

### What D2 does not yet cover

This is a partial CHE-372 delivery. Not implemented here: the full six-phase
cutover state machine's remaining phases (quiesce/stop-old-app/
recovery-capture/readiness/reconnect timing across the whole 60s envelope —
only the migration phase's own deadline is implemented), and
interrupted-concurrent-index-build recovery decisions. These are explicitly
D4 rehearsal and further D2 hardening work, not silently dropped scope — see
the CHE-372 issue thread for the acceptance-group breakdown.

The direct-database-consumer fence and the continuous sampling loop ARE
implemented: `migrate-supervised.sh`'s watchdog re-runs `final-gate` against
the verified-session registry (`--fenced-sessions`) on every iteration of its
own bounded loop (target period `fencing_interval_ms=250ms`, coverage
enforced by `fencing_max_sample_age_ms=1000ms`), with latching — any single
denied, stale, or unknown sample is immediately terminal for the run and is
never cleared by a later successful sample. `quiescence.mjs`'s exported
`SampleTracker` class predates that loop and is not used by it (or by
anything else in this tree); it also still implements the older
two-consecutive-miss/reset semantics the watchdog loop deliberately does NOT
use. Treat it as superseded/dead code, not as documentation of the current
sampling contract — the watchdog loop in `migrate-supervised.sh` is the
source of truth.

Decision-file preservation and startup validation across a restart are also
implemented: `migrate-supervised.sh` refuses to overwrite an existing
`--decision-file` unless its recorded decision is the non-terminal
`migration_started` progress marker (see the "Decision-file preservation
gate" comment block in that script), and only writes the terminal
`starting_candidate` contract `docker/entrypoint.cd.sh` waits on after the
final fencing re-check and the post-migration object-validity check both
pass. `entrypoint.cd.sh` itself treats `migration_started` as a pure progress
marker (continue waiting), never as a reason to exit — only a recognized
terminal decision (`starting_candidate` for the attempt it was given, or any
other terminal value as failure) ends its wait loop.

The main-push workflow produces a `build-evidence` manifest. Its configuration
digest is for the synthetic fixture only, so `admission.mjs` refuses it for a
deployment. A `release-candidate` manifest requires the fresh Hermes
configuration and migration snapshot and is the only manifest class that can
pass admission.

The optional fork changelog fetch runs in a separate job with `contents: read`
and `pull-requests: read`. Only its fetch step receives `GH_TOKEN`; neither the
image build nor C00 needs a GitHub credential for changelog entries. History
is bounded with `--shallow-since` from `FORK_CHANGELOG_START` (default
`2026-10-11`, midnight ICT), and PR merge commits are filtered against the
qualified SHA's first-parent history. The fetch summary reports shown and
hidden counts by reason. Fetch failure leaves no entries artifact and never
blocks the build.

The web image includes `apps/web/.fork-changelog/entries.json` when available.
The server page takes a module-load snapshot, preserving request-based locale
selection for upstream notes. Fork notes stay English. The header uses the
commit timestamp, displayed in ICT, and the first seven SHA characters; local
builds show `dev`. `cd-build-evidence.json` and `cd-image-provenance.json` bind
the exact entries bytes with `fork_changelog_sha256` (`sha256:<hex>`), or
`"absent"`. `release-manifest.mjs create --fork-changelog <path>` computes that
value; verification accepts `--expected-fork-changelog-sha256 <value>`.

`PR #20` (`CHE-392`) is an explicit prerequisite for running this workflow on
the dedicated X99 GitHub Actions runner. This change does not extend or
replace that pull request.

## Tuple rule

The manifest binds one source SHA, one configuration digest, one migration
inventory digest, the backend/web linux/amd64 image digests, and the SHA-256
of the read-only C00 tuple snapshot. Changing any member invalidates the
affected qualification evidence. The tuple is deployment-baseline metadata,
not a local configuration fixture: its Compose checksum and migration-ledger
checksum are never loaded as synthetic configuration. A later D2/D4 stage
must supply a fresh C00 configuration, live image, and migration snapshot
before it runs an upgrade or rollback rehearsal.

## Local checks

```bash
bash deploy/cd/test-release-manifest.sh
bash deploy/cd/test-admission.sh
bash deploy/cd/test-record-checks.sh
python3 deploy/cd/test-github-api-retry.py
python3 deploy/cd/test-candidate-preflight.py
python3 deploy/cd/test-deploy-approval.py
python3 deploy/cd/test-authorize-c00.py
python3 deploy/cd/test-post-deploy-readback.py
python3 deploy/cd/test-post-deploy-confirm.py
python3 deploy/cd/test-health-window.py
python3 deploy/cd/test-install-health-observer.py
python3 .github/scripts/test_ci_wait_policy.py
python3 deploy/cd/test-rollback-failed-readback.py
bash deploy/cd/test-isolated-qualification.sh
bash deploy/cd/test-tuple-snapshot.sh
bash deploy/cd/test-quiescence.sh
bash deploy/cd/test-migrate-supervised.sh
bash deploy/cd/test-entrypoint-compose.sh
bash deploy/cd/test-deploy.sh
bash deploy/cd/test-router.sh
bash deploy/cd/test-release-packet.sh
bash deploy/cd/test-cutover.sh
bash deploy/cd/test-artifact-controller.sh
node --test deploy/cd/drift-receipt.test.mjs
(cd server && go test ./internal/dbstartup/...)
```

The isolated qualification test uses only synthetic, throwaway data. Its
upgrade/rollback command is intentionally disabled until the caller supplies
an admitted tuple and a non-production fixture location.

## A/B application slots, router and cutover (CHE-397 unit 2)

Builds on D2 (above) to add the actual blue/green switch layer the accepted
CHE-397 architecture calls "controlled application replacement with
compatible image rollback" — two application slots with one active backend,
shared Postgres, shared uploads, never simultaneous serving.

- `docker-compose.ab.yml` — the slot overlay. Adds `backend-blue`/
  `frontend-blue`/`backend-green`/`frontend-green` on private
  `127.0.0.1` ports (`BACKEND_BLUE_PORT`/`BACKEND_GREEN_PORT`/
  `FRONTEND_BLUE_PORT`/`FRONTEND_GREEN_PORT`, defaulting to
  18081/18082/13001/13002), layered on the base `docker-compose.selfhost.yml`
  via Compose `extends` — invoke with the repository root as the working
  directory (`-f docker-compose.selfhost.yml -f deploy/cd/docker-compose.ab.yml`),
  never the other order or from `deploy/cd/` itself, or the `extends.file`
  path will not resolve (verified against a real `docker compose config`
  render). Each colour's `ports:` key uses the `!override` YAML merge tag,
  not a plain list — `extends` concatenates list-type keys by default, and a
  plain override would publish both the inherited base port AND the private
  one, which cannot both bind. Both colours share the base file's `pgdata`
  and `backend_uploads` volumes unchanged; there is no per-colour volume,
  database clone, or replica.
- `router/` — the local Nginx A/B router. `nginx.conf.template` is the
  per-generation config router.sh renders (a complete, independently
  `nginx -t`-validatable file); `docker-compose.router.yml`/`nginx.conf` are
  the long-running router container brought up once per host, whose own
  config never changes — only the generation file it `include`s does.
- `router.sh` — `select`/`revert`/`probe`/`validate`. Renders a complete
  config for a colour, validates it with `nginx -t` (via the same
  `nginx:1.27-alpine` image in dev and CI), atomically repoints the
  `active.conf` symlink (`ln -sfn`, a single `rename(2)`), reloads the
  running router (`nginx -s reload`, which drains old workers rather than
  dropping connections), and retains prior generations for `revert`. Never
  starts, stops, or recreates the router container itself.
- `deploy-lib.sh` — helpers `deploy.sh` (D2, single-slot) and `cutover.sh`
  (this unit) both source: JSON field reads, image reference parsing/
  digest verification, the migration one-shot runner, tuple capture. One
  implementation, not two that can independently drift the way CHE-549's
  finding 5 happened.
- `cutover.sh` — `cutover`/`rollback`/`status`. Extends deploy.sh's
  machinery (same lock file, same migration-ownership guarantees) rather
  than forking a second controller: quiesce via `quiescence.mjs`'s existing
  `preflight`/`final-gate`, verify the release packet via
  `release-packet.mjs`, run the standalone migrator against the INACTIVE
  colour's image, start that colour through the migration-free
  `MULTICA_SKIP_MIGRATIONS=1` path, require its exact `/health` commit and
  `/readyz`, activate the router generation via `router.sh select`, then
  stop the now-inactive colour. Rollback verifies the retained predecessor
  against the CURRENT schema ledger before starting it — never a down
  migration or backup restore as routine rollback; a ledger past the
  predecessor's last-known-good version is refused outright as
  MANUAL INTERVENTION / separately authorized recovery, not silently
  attempted.
- `release-packet.mjs` — the expand/contract release-packet contract check:
  ordered migration versions/checksums (verified against real files under
  `server/migrations/*.up.sql`, matching `server/internal/migrations.
  AllVersions()`'s own version-extraction exactly), ledger cleanliness
  (every ledger row must be in the packet's own ordered list and applied by
  an allowlisted writer), zero invalid indexes, all hooks/backfills
  `completed`, and a compatible previous image pair with digests plus a
  known `minimum_rollback_version`. This is an explicit extension of D2's
  contract, not a change to `release-manifest.mjs`/`tuple-snapshot.mjs` —
  D2 does not verify migration checksums today (see "What D2 does not yet
  cover" above); this module does, and only for the A/B cutover path.

### Port source of truth (CHE-773)

Slot ports have exactly one interpretation: the one Compose renders from
`.env`. `cutover.sh` reads every slot port from `docker compose config`,
exports it under the names `router.sh` reads, and checks the running
containers' `docker compose port` bindings against it. `router.sh` has no
fallback port and refuses when one is missing. Never read a slot port from a
shell `${VAR:-default}`: CD sources `.env` in a `bash -c` without exporting
it, so a child process sees none of it. That is how CD run 36023317861
health-checked green on 18082 while green listened on 18092.

Before draining the incumbent, `cutover` requires these to agree:
- the incumbent's bindings;
- the router's selected upstreams in `active.json`, in `active.conf`, and in
  the running router's `nginx -T`;
- the public route: `:8081/health` must reach the same backend process
  (commit + pid + started_at) as the slot port, and `:3000/` must answer.

The candidate's generation must also pass `nginx -t`. Any disagreement
refuses while the incumbent still serves.

Every stop that the single-active-backend invariant depends on (the drain,
the candidate stop during recovery, the rollback drain) is confirmed twice:
by Compose's exit status and by `compose ps --status running`. If either is
unconfirmed, nothing runs afterwards: no migration and no other colour. The
controller alerts instead. After `compose up`, the candidate's running
backend and web containers must use the exact local image IDs of the
digest-verified tags. A stale container causes recovery.

After the drain, a failure (final gate denial, candidate start/binding/
readiness/identity failure, router switch or public readback failure) runs
`recover_incumbent`: stop the candidate, prove the live ledger is the pre-
drain ledger or within `[minimum_rollback_version, packet final version]`,
`compose start` the incumbent's own stopped containers, and require the same
image pair, `/health` commit, router generation and public route as before
the drain. A failed migration or any unproven step does not restart
anything. Instead it prints `::error title=Multica A/B cutover outage::`,
writes `<state-dir>/outage-alert.json` and exits 1.

`ab-health-check.sh` is the bounded, read-only alarm: the router-selected
upstream `/readyz` and the public listeners must answer 2xx for the whole
`--window`. CD runs it for 120s after every cutover (0s after a failed one).

### Feature-flag runtime inputs (CHE-2348)

`docker-compose.flags.yml` preserves the feature-flag file across blue/green
recreation independently of Jev. `cutover.sh` loads this overlay for cutover
and rollback when the compose dir's `.env` sets `MULTICA_FEATURE_FLAGS_FILE`.
The existing `MULTICA_JEV_FLAGS_FILE` setting remains a fallback so C00 does
not need a configuration change. The generic setting takes precedence when
both are non-empty. With neither set, no flags overlay loads only if the
previous active backend has no `/app/feature-flags.yaml` mount. Otherwise
cutover and rollback refuse before any change with a `flags continuity`
error naming both settings. An unreadable container/mount inventory also
refuses before any change.

The host path must name an absolute, readable, non-empty file admitted by
`flagfile-check.mjs`. Validation runs before any drain, pull, migration or
start. Both backends mount the file read-only at `/app/feature-flags.yaml`,
with their `MULTICA_FEATURE_FLAGS_FILE` environment pointing to that mount.
The overlay does not set flag values, a Jev key, a gateway network or a
database hostname. A flags-only installation needs no Jev configuration.

After readiness and before switching the router, cutover and rollback read
the candidate backend's `GET /api/config` directly on its rendered slot port.
Every frontend-public flag present in the configured file must appear as
the matching boolean in `feature_flags`: `project_workflows_v1`, `plugins_v1`,
`composio_mcp_apps`, and `billing_workspace_subscriptions` (the allowlist in
`server/internal/featureflags/keys.go`). Missing flags, mismatches, malformed
responses and HTTP failures fail the operation without recording success.
The comparison uses the anonymous request's decision: allow/deny targets
do not match, and percent rules use the backend's deterministic FNV-1a
bucket for an empty identifier instead of the default value. Flags not in
the frontend-public allowlist are not compared. Environment overrides that
disagree with the file also fail this guard.

On a mismatch, the candidate is stopped before compatibility-gated recovery
restarts the original containers and verifies their identity and public
route. Rollback recovery requires the ledger to be unchanged since drain.
If recovery cannot be proven, the operation raises the existing outage
alert for manual intervention; it does not start a second backend or write
a success state.

### Scoped Jev runtime inputs (CHE-1854)

A backend recreated from the base files alone loses three things a scoped Jev
shadow runtime needs: the deployment key, the workspace-scoped feature-flag
file and the gateway network. `docker-compose.flags.yml` keeps the flag
file; `docker-compose.jev.yml` keeps the Jev key and network. Jev is
opt-in per installation: `cutover.sh` loads the Jev overlay only when the
compose dir's `.env` sets `MULTICA_JEV_GATEWAY_NETWORK`. Without that setting,
no Jev overlay loads. Jev stays
off everywhere else; the overlay never sets `FF_JEV_ENABLED` or `JEV_API_KEY`.

Protected operator inputs, all in the C00 compose dir's `.env` (never in the
repository, CI, logs or PR evidence):

| Variable | Meaning |
| --- | --- |
| `MULTICA_JEV_SECRET_KEY` | Deployment key: standard base64 of 32 bytes. Already passed through by `docker-compose.selfhost.yml`. |
| `MULTICA_FEATURE_FLAGS_FILE` | Absolute host path of a non-empty flag file. Bind-mounted read-only by the generic overlay at `/app/feature-flags.yaml`. |
| `MULTICA_JEV_FLAGS_FILE` | Existing host flag-path setting, used only when `MULTICA_FEATURE_FLAGS_FILE` is empty or unset. No C00 configuration change is required. |
| `MULTICA_JEV_GATEWAY_NETWORK` | Name of the existing external network. Both backends join it next to the project network. |

The flag file decides which workspace may use Jev. Shape (workspace ID is
yours, default stays `false`): `deploy/cd/fixtures/jev-feature-flags.synthetic.yaml`.
`flagfile-check.mjs` admits only a documented subset of the backend schema (the
shape in the file header; unknown fields, anchors, `yes`/`no` and unquoted
numeric list items, and `percent` values with leading zeros are refused; `percent` must also fit 64 bits even though the backend would accept some of
them). It runs before the first drain because both colours mount the same file
and the backend exits on a malformed one, so a restart of the incumbent could
not recover. The Go loader classifies the same corpus
(`deploy/cd/fixtures/flags`) in `server/internal/featureflags`.
Edit the file in place: replacing it by rename leaves a running colour on the
old inode until recreation.

Once the Jev gateway network is set, all three must hold. Before the first
drain, pull, migration or start, `cutover` and `rollback` refuse and name the
failing variable (never its value) when the key is not base64 of 32 bytes,
the selected flag file is not an absolute readable non-empty file that
`flagfile-check.mjs` admits, or the network does not exist. A refusal leaves the incumbent serving.

Joining the gateway network adds a second `postgres` to the backend's DNS
path. The overlay therefore reaches the database as `<project>-postgres-1`
(the effective Compose project: `-p`, `COMPOSE_PROJECT_NAME`, else `multica`),
which exists only on the project network. No IP pin or `extra_hosts` entry is used, so a changed
container address needs no edit. Ports, volumes, the blue/green routing,
the deploy lock, rollback and the one-active-backend rule are untouched.

Temporary-to-durable transition (operator, on C00; no value is printed):
1. Copy the key already in the protected backend environment into
   `.env` as `MULTICA_JEV_SECRET_KEY`, and configure the flag path and network.
2. The next CD cutover recreates the candidate colour with the overlay. The
   hand-made container from the temporary recovery is replaced by it; the
   original `unless-stopped` policy comes from the base files.
3. Confirm with `docker inspect` that the new backend lists the gateway
   network, the read-only flag mount and `MULTICA_FEATURE_FLAGS_FILE`, and
   that `/health` reports the new revision.

Source tests (`test-jev-overlay.sh`, `test-cutover.sh`) use synthetic values
and mocked Docker. They are not production or shadow acceptance.

### A/B outage runbook

For public 502s, a `Multica A/B cutover outage` annotation, or a failing
`ab-health-check.sh`, run everything on C00 from `$C00_COMPOSE_DIR`, with
`ab=(-f docker-compose.selfhost.yml -f deploy/cd/docker-compose.ab.yml)` —
except the router state directory below, which is wherever
`docker-compose.router.yml`'s `./state` bind mount actually resolved when
the router container was first adopted on C00 (a one-time, host-side
choice, recorded as `C00_ROUTER_STATE_DIR` in CD's own secrets; CHE-768
found it does NOT necessarily sit under `$C00_COMPOSE_DIR` — confirm the
real path with `docker inspect multica-ab-router --format '{{range
.Mounts}}{{.Source}} -> {{.Destination}}{{"\n"}}{{end}}'` rather than
assuming `deploy/cd/router/state`).

1. Record the state. Run `cat <state-dir>/cutover-state.json <state-dir>/outage-alert.json <router-state-dir>/active.json`,
   `docker exec multica-ab-router nginx -T | grep proxy_pass`, and
   `docker compose "${ab[@]}" ps -a`.
2. Confirm that at most one backend colour runs. If both run, stop the one
   the router does not select. Never run both on purpose: schedulers and
   realtime hubs are per-process.
3. Check schema compatibility before starting any colour. Read the ledger
   with `docker compose "${ab[@]}" exec -T postgres psql -U multica -d multica -tAc "select max(version) from schema_migrations"`.
   The incumbent can be restarted if the ledger equals its last-served
   ledger, or sits at or after `minimum_rollback_version` in
   `cutover-state.json` and no migration failed partway. If a migration
   failed, or you cannot tell, stop and escalate. Down migrations and
   backup restores need separate authorization.
4. Restart the router-selected colour's own containers with
   `docker compose "${ab[@]}" start backend-<colour> frontend-<colour>`. This
   reuses the stopped containers, so the image does not change.
5. Read back each of these, and do not stop at an HTTP 200:
   - `docker compose "${ab[@]}" port backend-<colour> 8080` equals
     `backend_port` in `active.json`.
   - `curl -s 127.0.0.1:8081/health` reports the expected commit.
   - `curl -sI 127.0.0.1:3000/` returns 200.
   - `bash deploy/cd/ab-health-check.sh --router-state-dir <router-state-dir> --cutover-state-dir <state-dir> --window 120` exits 0.
6. If the router selects a colour whose ports differ from Compose's render,
   re-select it with the rendered ports (`BACKEND_<C>_PORT=... FRONTEND_<C>_PORT=... bash deploy/cd/router.sh select --colour <colour>`).
   Take the ports from `docker compose "${ab[@]}" port ...`, never from memory.

### Deploy drift gate (CHE-2396)

The cutover bundle carries the released `docker-compose.selfhost.yml` and the
blue/green, flags and Jev overlays. Under the deploy lock, `cutover` copies
them into `--compose-dir` (`drift-receipt.mjs sync`) before anything renders.
After the candidate starts and before the router switches,
`drift-receipt.mjs check` writes `<state-dir>/drift-receipt.json` and exits
nonzero, failing the cutover (incumbent recovery), when any of these differ:

- deployed compose bytes vs the released source;
- the candidate containers' env key names vs the released compose render plus
  the image's own env (names only, never values);
- the candidate containers' image digests vs the manifest.

### What this unit does NOT cover

Real C00 execution (real forward switch, real rollback, controlled
connection-loss during a transition, in-flight agent task reconnect across
a cutover) is unit 3's job, under Hermes's C00 ownership — this unit is
build/test on X99 only, per its own scope. `router/docker-compose.router.yml`
bringing the router container up for the first time on C00, and the initial
Tailscale Serve upstream port handover (colour listeners moving off
8080/3000 onto the private ports this overlay defines) are also C00-side,
one-time adoption steps this unit does not perform.

`test-quiescence.sh` and `test-migrate-supervised.sh` each start their own
throwaway `postgres:16-alpine` container on a dedicated Docker network and
tear it down on exit; `test-migrate-supervised.sh` additionally builds
`server/cmd/migrate` with Go, runs the complete current migration set
against it, and runs the Go-level connection-string-bypass unit test via
`MULTICA_TEST_D2_BYPASS_DATABASE_URL`, so it takes longer than the other
checks. `test-entrypoint-compose.sh` builds the actual repository image
with the real `Dockerfile` and brings up `deploy/cd/d2-controller.compose.yml`
against it — the slowest of the D2 checks, since it does a full Docker
build. All three require Docker; `test-migrate-supervised.sh` skips
(rather than failing) its Go-dependent parts if no Go toolchain is found.

## Previous-image identity

The harness accepts either immutable GHCR digest references or a complete pair
of verified off-host OCI archives. Archive mode requires the compressed
archive's SHA-256 and a separately checksummed metadata file containing the
source reference, source image identity, platform, and ordered RootFS diff IDs.
It verifies the OCI index, config, and RootFS list before importing under a
unique local-only tag, then removes that tag during cleanup. Archive names and
Docker image IDs alone are not accepted as identity evidence.

To exercise the pre-merge gate without building or contacting a registry, use
the current Hermes tuple snapshot:

```bash
bash deploy/cd/premerge-synthetic-qualification.sh \
  --baseline-tuple deploy/cd/fixtures/c00-tuple-2026-09-12T235517Z.json
```

This creates a non-deployable `build-evidence` manifest in a temporary
directory. It is not a trusted-main image build and cannot qualify a release.

## D2 deploy and rollback (CHE-530)

`deploy.sh` and `.github/workflows/cd-deploy.yml` are the D2 controller: the
only pieces of this repository that hold C00 credentials and the only ones
allowed to mutate the live self-host stack. `cd-qualification.yml` (D1)
carries none of the secrets `cd-deploy.yml` uses — verify this after any
change to either workflow by diffing `cd-qualification.yml`; it should never
change as part of D2 work.

`cd-deploy.yml` runs automatically every 4 hours (00:17, 04:17, 08:17, 12:17,
16:17, 20:17 ICT; CHE-5182), and can still be dispatched by hand. Its first
job, `schedule-resolve`, picks the newest `main` commit whose `cd-qualification`
run succeeded. When C00 already runs that commit or a descendant, the run ends
there with "nothing new to deploy": no C00 runner, lock or approval is taken.
The automatic path adds `candidate-preflight` and `prepare-release-candidate`, which the manual
path does not need (CHE-4582: the automatic path deploys only for an
authorized merger, below). The resolved candidate SHA is in the `schedule-resolve` job summary.
GitHub can drop a scheduled slot. When no `schedule` run exists 30 minutes after a slot,
the freshness alarm (`cd-freshness-watch.yml`) stays red and sends one
`gh workflow run cd-deploy.yml --ref main -f schedule_backup=true` (CHE-5353), skipped when any
`cd-deploy.yml` run on `main` was created since the slot. The `schedule_backup` marker makes that
run take the scheduled path: `schedule-resolve`, `candidate-preflight`, the merger check and the
suspension check, exactly as a scheduled run. `authorize-c00` accepts the marker only from
`github-actions[bot]` and refuses any other sender; the owner dispatches without the marker.

`candidate-preflight` runs on X99 without a deployment environment or
C00 credentials. It uses the workflow revision's tools so an older candidate
does not need to contain the new preflight script. Before requesting approval,
`require-current.sh` requires the candidate to be an ancestor of (or equal to)
`main` and a strict descendant of C00's running commit (CHE-2159). A qualified
older `main` commit is eligible even when a newer commit has pending or passing
checks. Same-version, downgrade, divergent and off-main candidates are skipped.
The guard reads C00's public `/api/config` over verified HTTPS using the
repository Actions variable `C00_CONFIG_URL`, without SSH or deployment
credentials. Set that variable to the live C00 HTTPS `/api/config` endpoint
reachable from the X99 runner before enabling this workflow revision. An unset
URL fails closed; the endpoint is not embedded in the public repository. The
reported short `server_commit` is resolved to a full SHA through GitHub; missing,
malformed, ambiguous or unknown versions fail closed. The bounded version read
and ancestry checks run before and after waiting for required checks. The
recorder keeps the newest run of each check, so a green re-run replaces the
failure it re-ran. A non-forward candidate or non-admissible check
result leaves `eligible=false`; `prepare-release-candidate` and the automatic
admission/deploy chain are skipped, not failed. The summary records the real
candidate SHA and the skip reason. API or tooling errors still fail closed.

`cd-qualification.yml` groups pushes by `github.ref` with
`cancel-in-progress: false`: the running qualification build finishes, and a new
`main` push replaces only the older pending run, so at most one build runs and
one waits. The newest commit can wait for the running build (about 25-45 min).
A finished older build is not wasted: `require-current.sh` accepts a qualified
older `main` commit that is a forward move from C00. This group is separate from
every approval and C00 mutation lock.

The owner's merge deploys (CHE-4582, D5 = A); everything else builds and stops
(CHE-4495). After a successful qualification, `authorize-c00` asks the GitHub
API which pull request has the commit as its `main` merge commit, then reads
that pull request's own record for `merged_by` (the commit's pull list has none).
If the merger is `congvc-dev` or a login in the repository variable
`C00_DEPLOY_AUTHORIZERS` (comma list; CHE-3123 sets the agent merge identity
there, no workflow change), and `candidate-preflight` found the commit
eligible, `prepare-release-candidate`, `admission` and `deploy` run with no
dispatch click. A merge by anyone else, or a commit with no merged `main` pull
request, runs `candidate-preflight` and `notify-approval` and stops: no C00 job
runs. The notice links to the workflow's dispatch page with the full
`candidate_sha`. The notice goes by `C00_APPROVAL_WEBHOOK_URL` and/or a Multica
decision card (secret `C00_APPROVAL_MULTICA_TOKEN`; the server URL, workspace, issue, member and anchor comment default in the workflow, and secrets `C00_APPROVAL_MULTICA_{SERVER_URL,WORKSPACE_ID,ISSUE,MEMBER_ID,SOURCE_COMMENT}` override them. The token holds only that scope, so the job never reads comments; the anchor is a fixed root comment of the notice issue:
`POST /api/decisions/ci` opens one card on that issue for the member with the
dispatch one-liner and the run link). `TOKEN` is a personal access token of the
workspace owner with the `decisions:ci_request` scope. The server keeps one open
card per SHA, withdraws the older open card when a newer candidate arrives and
refuses a stale one with 409 (the job treats 200 and 409 as delivered). The job
runs in the `c00-approval-notice` concurrency group so notices do not overlap.
With no route configured or delivered the job fails
(CHE-4879). The `approve-c00` environment approval was removed: required
reviewers do not exist on a private Team repository, so it was a silent no-op.

The gate is `authorize-c00` (`deploy/cd/authorize-c00.py`, checked out from the
workflow revision, never from a pull request). It has no environment and no C00
secrets. For `workflow_dispatch` it exits 1 unless `github.triggering_actor` is
`congvc-dev` or in `C00_DEPLOY_AUTHORIZERS`. For a qualification it never
fails for a non-authorized merger: it outputs `authorized=false` and every C00
job is skipped; a GitHub API error fails the job closed. Every C00-touching job
(`prepare-release-candidate`, `admission`, `deploy`) keys on its
`authorized` output, so another actor's dispatch fails before any C00 step.
The `c00-runtime` deployment-branch policy allows `main` only, so a dispatch
from another ref gets no `c00-runtime` secrets.

After `deploy` succeeds, C00 is watched for 15 minutes without holding a runner
(CHE-2365, CHE-5175), in three jobs outside the C00 lock:

1. `readback` runs `post-deploy-readback.sh` and holds the C00 runner for at most
   2 minutes. It waits for the public route and upstream to turn healthy (one
   sample per try, `READBACK_RETRY_SECONDS` apart), then reads the running commit.
   A descendant of the deployed commit means a newer deploy already landed: the
   readback passes without arming, and that deploy's own journal and digest check
   apply. Otherwise it **arms the health observer** (`health-window.mjs arm`,
   journal `OBSERVATION_ID` = `<run id>-<attempt>`). Under the observer's lock, arm
   re-reads the live commit (`live-application-sha.sh`); if it is no longer the
   deployed one, arm exits 3 and touches no journal, and the readback re-reads and
   passes for a descendant or fails. Arm records T0 (epoch seconds) and the container
   identity, takes slot 0, and does not return until the native systemd timer has
   taken a passing sample (`READBACK_ARM_VERIFY_SECONDS`, default 30). If the timer is
   not installed or does not fire, the readback fails and nothing is watched
   unobserved. It then checks the image digests against the admitted manifest, and
   records the active colour's Docker restart count (`restart-count.sh`). T0, the
   observation id and the restart count are job outputs.
2. `readback-delay` is the 15-minute wait timer of the `c00-readback-delay`
   environment. A waiting job holds no runner. It runs on a hosted runner
   because a job can use only one environment and the C00 secrets stay in
   `c00-runtime`.
3. `readback-confirm` runs `post-deploy-confirm.sh` on the C00 runner for about
   10 seconds. It verifies the journal and the events; it does not watch.

The observer is `multica-health-observer.timer` on C00 (installed once by
`install-health-observer.sh STATE_DIR RUN_USER`, which is C00 configuration and
needs the owner's approval). Every 5 s it runs `health-window.mjs tick`, which does
nothing until a readback arms an observation. For the armed observation it takes
one sample per 10 s slot, T0 to T0+900 inclusive (91 samples), by running
`ab-health-check.sh --window 0` (selected backend `/readyz`, routed API `/health`,
routed web `/`, each with a bounded timeout). The journal is
`$C00_STATE_DIR/health-observer/<observation>/` (`meta.json`, `samples.jsonl`,
`state.json`); each sample records its scheduled slot, actual start and end, the
source (`arm`, `timer` or `manual`), the systemd invocation id, the active colour,
the container ids and the result. A failed sample stays failed after recovery. The
observer holds no CI lock, runner or agent run.

`post-deploy-confirm.sh` fails (and so suspends automatic deploys and starts the
rollback) unless: at least `CONFIRM_WINDOW_SECONDS` (900) passed since T0; C00 runs
the deployed commit with the admitted image digests, or runs a newer deploy that
descends from this one (that deploy's own journal and confirm then judge its own
window, and the older window is not judged); the journal verifies (`health-window.mjs
verify`: present, readable, not truncated, this deploy's id and commit, not
superseded, no replaced containers or identity change, exactly one passing sample
in every slot including the last, each taken by the native timer, slot 0 by the arm step); the
route answers once more; `docker events` since T0 shows no `health_status:
unhealthy`, `die`, `oom` or `restart` for the active colour's backend and frontend;
and the restart count did not grow. A newer deploy arms its own observation, which
supersedes an older open journal; journals are never mixed. An older readback that
reaches arm after a newer deploy is refused (exit 3) and never supersedes it.

The wait timer starts when `readback` finishes, after T0, so the last slot (T0+900)
is normally sampled before `readback-confirm` starts. If it is not, verify fails
closed rather than pass an unobserved slot.

`c00-readback-delay` is a repository setting, not workflow code: deployment
branches `main` only (like `c00-runtime`), a wait timer of 15 minutes, no
secrets and no reviewers. Create it before the first deploy after this change;
without it the first run creates the environment with no wait timer and the
window is not watched.

A failed `deploy`, `readback`, `readback-delay` or `readback-confirm`
suspends automatic deploys: the next `authorize-c00` walks the finished `main`
runs of this workflow, newest first, 100 per page and at most 5 pages, until one
had a `deploy`/`readback`/`readback-delay`/`readback-confirm` job that succeeded or failed. A failure there means
suspended, however many built-and-stopped runs came since. Finding no deploy in
all 5 pages is unknown and stays suspended; a history that ends sooner (never
deployed) is not suspended. A later successful deploy, such as the owner's
dispatch, lifts it; a dispatch is never suspended. A failed cutover rolls back
by digest inside `cutover.sh`. A failed `readback`, `readback-delay` or `readback-confirm` also starts the `rollback`
job, under the C00 lock: `rollback-failed-readback.sh` runs `cutover.sh
rollback` (retained predecessor, no down migration, no restore) and reads back
the health and SHA. It identifies the live commit with `live-application-sha.sh`,
from the active colour's container (running or stopped), so a stopped colour
does not block the rollback. It skips when C00 runs another commit, and fails
without rolling back when the commit cannot be identified. It pins the
retained predecessor's own image repo and tag, read from its containers, after
sourcing `.env`, so a stale `.env` tag never decides what restarts; it fails
without rolling back when that image is unknown or the tags differ. The
run stays red, so the suspension holds. A rollback refused by the controller
(ledger below `minimum_rollback_version`, failed gate) fails the job and needs a
person. Merge-queue merges report the enqueuer or merger as
`merged_by`; a merge by a bot not on the list builds and stops.

An empty dispatch (no `candidate_sha`, no evidence path) deploys the newest
`main` commit with a successful qualification push run: `resolve-candidate`
picks it after `authorize-c00` and prints it in the job summary (CHE-4998); with
none it fails. When that commit is behind `main`, the summary says how many
commits and since when, with the newest `main` qualification run link; more than
20 commits or 2 h behind adds a `::warning` and one line on CHE-4960, posted by
`notify-stale-candidate` on X99 (`post-stale-notice.sh`; needs the
`CD_NOTICE_MULTICA_TOKEN` secret, warns only without it). The deployed commit is
unchanged (CHE-5352). An authorized qualification passes its own head SHA through the
same job, so every job behind it reads one `sha` output.
A dispatch with
`candidate_sha` looks up the successful `main` push qualification run and issues
the release candidate itself; manual evidence paths remain for explicit
admitted manifests. The `c00-production` environment is no longer used by this
workflow; its secrets can be retired. The
`prepare-release-candidate` and `deploy` jobs use the C00 secrets from the
`c00-runtime` environment.
A missing `c00-runtime` environment fails closed: GitHub creates it empty, the
deploy step has no secrets and stops. They share the
job-level lock `cd-deploy-c00` with `queue: max` and no `cancel-in-progress`: one
C00 stage at a time, queued FIFO, and a started deploy is never cancelled. The
default queue (`single`) keeps one pending job and cancels it for any newcomer,
whatever its version, so a stale stage joining late would evict the newest
candidate. A freshness check made before the join cannot prevent that, because
check and join are not atomic. `max` holds up to 100 pending jobs and evicts
none; if the queue is full GitHub cancels the newcomer. Once `deploy` holds
the lock, `capture-remote-baseline.sh` re-reads C00's live tuple using the same
image-label-first capture path as preparation. `verify-deploy-baseline.mjs`
skips only when C00's running commit already contains the candidate (equal
or descendant). Unknown or divergent history fails closed. A newer commit
on `main` alone does not skip an approved deploy.

Before building or transferring any controller bundle, the live tuple must
match the admitted baseline in every field except `captured_at`. Image,
configuration, application or migration-ledger drift fails closed without
changing the running stack or transferring the stale manifest. The operator
must prepare and admit a fresh release candidate after such a failure; the
workflow does not silently rebind approved evidence. Both automatic and manual
deploys use this guard. `actionlint` does not know the `queue` key
yet and reports it; GitHub validates it on the first run. There is no workflow-level lock, since a run waiting for approval would hold it and
block every newer version.

Preparation still records a fresh check snapshot after approval; preflight
evidence does not replace final admission. Manual dispatch keeps its existing
evidence and admission requirements and bypasses only the automatic
eligibility filter.

The `deploy` job runs directly on the C00 self-hosted runner (`[self-hosted, linux, x64, cheese-c00-deploy]`) and stages the content-addressed cutover bundle into a fresh `remote_dir` (`${C00_STATE_DIR%/}/cd-deploy-<ts>`), verifies `cutover-controller.tar.sha256` and `verify-cutover-bundle.sh`, and executes `cutover.sh` locally through `render-cutover-remote-script.sh | env -i HOME="$HOME" PATH="$PATH" bash -s` without opening an SSH or SCP session or leaking ambient step environment variables into the cutover shell.

D1's automatic main-push build only ever emits a `build-evidence` manifest,
which `admission.mjs` refuses. A deployable `release-candidate` additionally
binds the baseline tuple of the host being deployed to — the fresh Hermes
configuration digest and migration snapshot described above — which only a
stage running on C00 can read. `prepare-release-candidate` runs on the
C00 self-hosted runner under the `c00-runtime` environment as the
deploy job, after the `authorize-c00` gate, reads that baseline locally with `capture-tuple.sh`, re-issues
D1's evidence (carrying D1's own image digests forward unchanged) as a
release-candidate bound to it, records the qualifying event and the commit's
required-check results, and hands the set to the unchanged `admission` and
`deploy` jobs.

D1 completing does not mean CI has: `backend` is a `needs:`-gated job whose
check run does not exist until the jobs it aggregates finish, often well after
`cd-qualification`. `record-checks.sh` therefore polls the commit's check runs
until `admission.mjs pending` reports every required check terminal (or
provably path-excluded), up to an hour, before it writes `cd-checks.json`. It
waits before reading C00's baseline so the tuple is fresh. A terminal failure
or cancellation ends the wait at once and admission refuses it; a check still
running at the deadline is recorded as running and refused as "has not
completed". Run 35822016641 (`5c834a2f`) is the case: the snapshot at
05:37 UTC had no `backend` run, which then passed at 05:55.

The commit and check-run reads retry HTTP 403 rate limits and HTTP 429,
honoring `Retry-After` or `X-RateLimit-Reset` within the checks deadline.
Ordinary permission errors and a reset beyond the deadline fail closed.
Each paginated response is buffered until every page succeeds, so a failed
page cannot leave a partial check snapshot. `candidate-preflight.sh` shares
one deadline across its current-candidate checks and the recorder through
the internal `CHECKS_DEADLINE` variable.

Before Buildx setup, qualification inspects only its named persistent
builder. A failed bootstrap, node error, or incompatible driver removes that
builder so setup can recreate it. Healthy builders retain their cache.

`admission.mjs` is not weakened to make this fit: it still refuses
build-evidence, still requires the manifest to bind the exact tuple it is
verified against, and still requires `backend`, `frontend`, `mobile` and
`cd-qualification` to be `success` for that SHA. Verified against the real
qualified artifacts of `main` commit `abe17e2b` and C00's live baseline:
admission returns `{"admitted":true}`, while raw D1 build-evidence, a
manifest bound to a different tuple, and a failing required check are each
refused.

### The deployed tuple is the next deploy's baseline

`deploy.sh` records the deployed tuple through the same `capture-tuple.sh`
the preparation job uses, so "what C00 is running" has exactly one
implementation and the tuple a deploy writes is admissible as the next
deploy's baseline. This matters more than it looks: `capture_tuple` used to
build that JSON inline with placeholder digests that `tuple-snapshot.mjs`
rejects (61 hex characters where it requires 64), which would have made the
automatic chain work exactly once and then fail admission on a tuple it
wrote itself. `deploy/cd/test-deploy.sh`'s `deployed-tuple-is-admissible`
scenario asserts the written state file passes the validator and carries
real digests.

### Where and when migrations run (acceptance item 5)

Migrations run in exactly one place per deploy: a dedicated one-shot
container `deploy.sh` launches before touching the `backend` or `web`
services:

```bash
docker compose run --rm --no-deps --entrypoint ./migrate backend up
# or, during rollback:
docker compose run --rm --no-deps --entrypoint ./migrate backend down --to <version>
```

This container is built from the exact backend image being deployed (or, for
a rollback, the previous good image — its `migrate` binary is the one that
shipped with the down-migration files being applied, and is guaranteed to
know how to reverse them; the new/failed image's binary may not). It joins
the already-running compose network and reads the same `DATABASE_URL` the
rest of the stack uses, but its entrypoint is overridden straight to
`./migrate` — it never reaches `docker/entrypoint.sh`.

Only after that one-shot container exits `0` does `deploy.sh` run
`docker compose up -d --no-deps backend web`, and it starts those services
with `MULTICA_SKIP_MIGRATIONS=1`. `docker/entrypoint.sh` checks that variable
before running its own `migrate up` step (see the comment at its top): when
set to exactly `1`, it skips straight to `exec ./server`. Unset — the default
for every other environment, including a plain `docker compose up` a
self-hoster runs by hand — the entrypoint's own migration step runs exactly
as it always has; this is unit-tested behaviorally in `scripts/entrypoint.test.sh`
(stub `migrate`/`server` executables under the real `docker/entrypoint.sh`,
asserting the guard unset still runs migrations then starts the server, and
`MULTICA_SKIP_MIGRATIONS=1` skips migrate but still starts the server) —
run it with `bash scripts/entrypoint.test.sh`. This is a shell test alongside
`docker/entrypoint.sh` itself, not part of `cmd/migrate`'s Go test suite.

This is what makes "migrations never run twice or race a second container"
provable rather than asserted:

1. The one-shot migration container's exit code gates whether
   `docker compose up -d` runs at all (`deploy.sh`'s `run_migration_step`
   call site) — a failed migration step never reaches the point where any
   application container starts.
2. Every application container in the stack starts with
   `MULTICA_SKIP_MIGRATIONS=1` set by `deploy.sh`, so no `backend` container
   — original or any future replica — re-enters `docker/entrypoint.sh`'s own
   migration step. A restart or scale-up of `backend` alone (outside a
   `deploy.sh` run) still runs `entrypoint.sh` fresh without that variable
   and therefore still self-heals via the ordinary `migrate up` no-op path
   (already-applied migrations are skipped, see `cmd/migrate`'s
   `schema_migrations` ledger check) — it does not need the guard to be
   correct, only to avoid a redundant real migration attempt racing
   `deploy.sh`'s own one-shot step while a deploy is in flight.
3. `deploy.sh` takes an `flock` on a state-directory lock file before doing
   anything, so two `deploy.sh` invocations (e.g. a manually re-triggered
   workflow run) cannot interleave their one-shot migration steps or
   container restarts against each other. `cd-deploy.yml`'s own
   the job-level `concurrency: group: cd-deploy-c00` (on
   `prepare-release-candidate` and `deploy`) is the first line of defense for
   normal workflow dispatches; the lock is the second, host-local one for
   anyone driving `deploy.sh` by hand.

### Bounded rollback

`server/cmd/migrate` gained a `down --to <version>` mode for this issue
(`server/cmd/migrate/main.go`, tested in
`server/cmd/migrate/migrate_bounded_rollback_test.go`). Bare `migrate down`
with no `--to` is unchanged: it still walks every applied migration back to
version 001, exactly as it always has. `--to <version>` stops as soon as it
reaches (but does not roll back) the named version, and errors instead of
running the unbounded walk if that version is not found among the known down
migrations — silently falling back to the full reverse walk on a typo'd or
unrecognized `--to` value would be exactly the overshoot this flag exists to
prevent.

`deploy.sh`'s rollback path computes its `--to` target from the previous
deployed tuple's recorded `migration_ledger.latest.version` (the same shape
`tuple-snapshot.mjs` already validates — see `deploy/cd/fixtures/`), so a
failed deploy's rollback always targets the exact schema version the
previous, healthy deploy left the database in — never further back, and
never an arbitrary guess at "how many migrations were new".

Every version 441–474 (the current HEAD's most recent ~35 migrations,
covering both the ~460–474 window a realistic rollback deploy would touch
and a wide margin around it) has been verified against a real
`pgvector/pgvector:pg17` container: migrate up to HEAD, bounded rollback to
441, migrate back up to HEAD again, with no errors in either direction. Any
down-migration below that tested window is unaudited by this change; treat
an untested `--to` target below 441 as unverified until it is exercised the
same way.

### What is NOT verified by CI or local testing

`deploy.sh` and `cd-deploy.yml` have not been run end-to-end against the real
C00 host by a real workflow run. What HAS been verified against real systems:

- The `prepare-release-candidate` → `admission` chain, run by hand with the
  same commands the job runs: real D1 artifacts downloaded from the
  `cd-qualification` run for `main` commit `abe17e2b`, a real baseline tuple
  captured from the live C00 stack, real check-run results from the API.
  Admission returned `{"admitted":true}`; the three negative cases above were
  each refused.
- `capture-tuple.sh` against the live C00 stack, read-only, producing a tuple
  that passes `tuple-snapshot.mjs` with real image, compose and ledger
  digests.
- The bounded-rollback Go tests and CLI runs against a real throwaway
  Postgres, and `deploy.sh`'s full control flow (pull → digest-verify →
  one-shot migrate → `up -d` → health-check → rollback-on-failure, including
  which image tag each step uses and that the recorded tuple is admissible)
  against a scripted mock of `docker`/`docker compose`/`curl`.

Still unverified, and blocked rather than untested:

- **A real deploy run.** The `cheese-c00-deploy` runner that
  `prepare-release-candidate` and `deploy` require is not currently
  registered — the repository's runner list is empty. Until a runner carrying
  that label is online, the automatic path cannot execute regardless of the
  workflow being correct.

**Pulling the qualified pair on C00 (CHE-549) is fixed, not blocked.** GHCR
read was denied to every credential available on C00 — the org/App
`packages: read` permission governs the GitHub API, not an unauthenticated
`docker pull`, and there is no per-package "add an App" control for a
private container package (only repositories can be added to that list).
The fix is a registry credential rather than any App/org permission change.
CHE-549 first used a `read:packages`-only classic PAT stored as the
`GHCR_PULL_TOKEN` environment secret. CHE-2027 replaced it: the `deploy` job
now has `packages: read` and passes its own `${{ github.token }}` as
`GHCR_PULL_TOKEN`, so no PAT is stored, rotated or revoked. This needs each
package (`multica-backend`, `multica-web`) to list `cheese-work/multica` under
Package settings, Manage Actions access, with role Read. The token is scoped
to the job, so D1's `cd-qualification.yml` stays credential-free.
`deploy.sh` logs in to `ghcr.io` with that token immediately before the pull step
and cleans up on exit — success, rollback, or an early
argument/file-check failure — via a single EXIT trap; unset, both steps are
skipped (local `--dry-run` runs, or a host with its own credential store).
The login uses a private temporary Docker configuration and a credential
helper that reads the job token from the environment. The helper never
stores the token on disk. Exit cleanup removes only that configuration and
helper and restores the caller's Docker configuration and executable path.
`cd-deploy.yml`'s `deploy` job forwards the secret to the local cutover shell
by exporting it inside a script piped over the shell's stdin, never as a
command-line argument or a file written to C00, so it is not visible to
`ps` or left behind by the `remote_dir` cleanup. Covered by
`deploy/cd/test-deploy.sh`'s `ghcr-login-happy-path` and `ghcr-login-fails`
scenarios (mocked `docker login`/`logout`; a real end-to-end pull against
GHCR is still pending the runner above).
`deploy/cd/test-ghcr-credentials.sh` checks the helper protocol, absence of
on-disk credentials, cleanup after successful and failed login, and restoration
of the caller's environment without contacting a registry.

## Rollback-drill candidate evidence and readiness fault (CHE-4664)

`cd-evidence.yml` (owner dispatch only) issues `cd-release-candidate-<sha>`
for an already-qualified `main` commit and stops. It has no admission, deploy
or cutover job and never builds. Its steps are those of `cd-deploy.yml`'s
`prepare-release-candidate`, so its one C00 access is the read-only
`capture-remote-baseline.sh`. The `c00-runtime` environment allows only
`main`, so the workflow must be on `main` and dispatched with `--ref main`:

```
gh workflow run cd-evidence.yml --repo cheese-work/multica --ref main -f candidate_sha=<40-hex sha>
```

`docker-compose.fault-readyz.yml` points the candidate backend at
`fault-db.invalid` (reserved TLD, never resolves). `cutover.sh` loads a fixed
overlay list (ab, flags, jev) and has no hook for it, so `deploy/cd/drill/docker`
carries it in: a `docker` shim that appends the overlay to one call, the
candidate start (`compose ... up` with `MULTICA_SKIP_MIGRATIONS=1`). The
migrator (`run`), the incumbent recovery (`start`) and everything else pass
through, so the migration reaches the real database. A bad URL at the migrator
would fail the migration after the incumbent was drained, and `cutover.sh`
treats that as an outage with neither colour running. `cutover.sh` is not edited.

Drill procedure (the operator runs the real cutover; this shim changes nothing
else):

```
export DRILL_FAULT_READYZ=1
PATH="<repo>/deploy/cd/drill:$PATH" deploy/cd/cutover.sh cutover <the usual arguments>
```

The shim is inert without `DRILL_FAULT_READYZ=1` and refuses (exit 2) if the
candidate start carries no `-f` files. It prints `drill shim: layering ...` on
stderr once, at the candidate start: that line is the proof it fired. Unset the
variable and the PATH prefix afterwards.

Expected `cutover.sh cutover` outcome with the overlay applied: final gate and
migration pass; the candidate container starts, then exits with `unable to
connect to database` / `unable to ping database` (the service restarts under
`unless-stopped`); `/readyz` never answers; `wait_ready_on_port` gives up
after 180 s (an earlier bindings check may fire first); `recover_incumbent`
restarts the incumbent; the router generation never switches. This exercises
`recover_incumbent`, not the `rollback` subcommand.

`test-evidence-fault.py` checks the workflow shape, renders the overlay, and
drives `deploy-lib.sh`'s real `compose()` function through the shim against a
stand-in `docker`: the candidate start renders the fault, the migrator, the
recovery start and a run without the switch render the real database. It does
not run `cutover.sh` or touch C00.
