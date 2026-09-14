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

The absolute deadline covers cancellation too: the cancel-confirmation
window is bounded by the same `deadline_epoch` computed once at the top of
the script, never by a fresh `now + reserve_seconds` clock started at
cancellation time (which would silently re-grant the reserve every time
cancellation itself took any time to notice the deadline had passed). A
nonzero migrator exit is captured explicitly (`set +e` / `set -e` bracket
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
only the migration phase's own deadline is implemented), the direct-
database-consumer fence beyond HTTP, the sampling loop (`SampleTracker` is
implemented in `quiescence.mjs` but not yet wired into a continuous
migration-phase sampler), interrupted-concurrent-index-build recovery
decisions, and crash/restart recovery of controller state. These are
explicitly D4 rehearsal and further D2 hardening work, not silently dropped
scope — see the CHE-372 issue thread for the acceptance-group breakdown.

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
