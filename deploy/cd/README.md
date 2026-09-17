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
bash deploy/cd/test-isolated-qualification.sh
bash deploy/cd/test-tuple-snapshot.sh
bash deploy/cd/test-quiescence.sh
bash deploy/cd/test-migrate-supervised.sh
bash deploy/cd/test-entrypoint-compose.sh
(cd server && go test ./internal/dbstartup/...)
```

The isolated qualification test uses only synthetic, throwaway data. Its
upgrade/rollback command is intentionally disabled until the caller supplies
an admitted tuple and a non-production fixture location.

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

`cd-deploy.yml` runs automatically after a successful `cd-qualification` run
for a `main` commit, and can still be dispatched by hand. The automatic path
adds one job the manual path does not: `prepare-release-candidate`.

D1's automatic main-push build only ever emits a `build-evidence` manifest,
which `admission.mjs` refuses. A deployable `release-candidate` additionally
binds the baseline tuple of the host being deployed to — the fresh Hermes
configuration digest and migration snapshot described above — which only a
stage with C00 access can read. `prepare-release-candidate` runs on the
C00-reachable runner under the same `c00-production` environment as the
deploy job, reads that baseline over SSH with `capture-tuple.sh`, re-issues
D1's evidence (carrying D1's own image digests forward unchanged) as a
release-candidate bound to it, records the qualifying event and the commit's
required-check results, and hands the set to the unchanged `admission` and
`deploy` jobs.

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
   `concurrency: group: cd-deploy-c00` is the first line of defense for
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

Still unverified, and both are blocked rather than untested:

- **A real deploy run.** The `cheese-c00-deploy` runner that
  `prepare-release-candidate` and `deploy` require is not currently
  registered — the repository's runner list is empty. Until a runner carrying
  that label is online, the automatic path cannot execute regardless of the
  workflow being correct.
- **Pulling the qualified pair on C00.** GHCR read is denied to every
  credential available to this host, so no stage can pull
  `ghcr.io/cheese-work/multica-*:sha-<commit>` yet. Fixing it needs either
  `packages: read` added to the `congvc-bot` App installation or a PAT with
  `read:packages`.
