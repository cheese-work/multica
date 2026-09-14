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

Timeout proof (`set-role-timeout-defaults` / `verify-role-timeout-defaults` /
`find-live-session-by-role` / `verify-live-session-is-covered`) does not
attempt to read back another live backend's session-level GUC value —
PostgreSQL provides no view for that; `pg_stat_activity` carries no such
column, and `pg_settings`/`current_setting()` only ever report the caller's
own session. Instead it sets and verifies a role/database-scoped default via
`ALTER ROLE ... IN DATABASE ... SET`, recorded in the world-readable
`pg_db_role_setting` catalog, which PostgreSQL applies to every **new**
connection that role opens against that database from the moment it is set —
covering the migrator's pinned advisory-lock connection and every
separately-opened hook connection its pool opens afterward alike, not just
whichever one connection a client-side probe happened to inspect. A value
that rounds to 0 (Postgres's "disabled" sentinel) is treated as a hard
failure, and a role/database with no default configured at all denies rather
than being treated as "no timeout configured is fine." After launch,
`verify-live-session-is-covered` confirms the live migrator session's
identity (Postgres backend PID + `backend_start`, not the OS-level child
process ID) actually matches the role/database the default was verified for,
closing the gap where the default could be proven for the wrong session
entirely (wrong role, wrong database, or a stale/reused backend PID).

Every observation query is a fresh, autocommit `psql` invocation (no
persistent connection, no long-lived snapshot from the observer itself),
bounded by both a server-side `statement_timeout` and a wall-clock `timeout`
wrapper. On a host without `psql` on `PATH` (the case on X99 CD runners),
pass `--psql-via-docker-network <network>` to run each observation through a
short-lived `postgres:16-alpine` container on the same Docker network as the
target, mirroring D1's existing synthetic-fixture convention.

`migrate-supervised.sh` wraps `server/cmd/migrate` with the absolute
`migration_deadline`/`work_deadline` arithmetic from the design
(`min(migration_started+15s, cutover_started+40s) - 2s` reserve), sets and
verifies the role/database timeout default described above before launch,
confirms the live migrator session's identity immediately after launch, and
runs an external watchdog that can terminate only its own recorded migrator
PID — confirmed via process liveness, never trusted from a signal's return
code alone — and never a foreign session or anything matched by executable
name. A deadline-exceeded run always exits `3` (`needs_operator`) and never
claims success; it does not attempt to repair or roll back the database
itself.

`docker/entrypoint.cd.sh` is a new, explicit deployment-mode entrypoint that
gates `exec ./server` behind an external decision file the D2 controller
writes, instead of the stock `entrypoint.sh` unconditional migrate-then-serve
chain. `entrypoint.sh` itself is unchanged and remains the default for
non-CD use.

### What D2 does not yet cover

This is a partial CHE-372 delivery. Not implemented here: the full six-phase
cutover state machine (quiesce/stop-old-app/recovery-capture/migrate/
readiness/reconnect timing across the whole 60s envelope), the direct-
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
```

The isolated qualification test uses only synthetic, throwaway data. Its
upgrade/rollback command is intentionally disabled until the caller supplies
an admitted tuple and a non-production fixture location.

`test-quiescence.sh` and `test-migrate-supervised.sh` each start their own
throwaway `postgres:16-alpine` container on a dedicated Docker network and
tear it down on exit; `test-migrate-supervised.sh` additionally builds
`server/cmd/migrate` with Go and runs the complete current migration set
against it, so it takes longer than the other checks. Both require Docker;
`test-migrate-supervised.sh` skips (rather than failing) if no Go toolchain
is found.

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
