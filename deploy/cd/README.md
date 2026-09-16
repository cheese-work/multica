# D1 image qualification

This directory contains the build-side half of CHE-372's deployment design.
It builds a linux/amd64 backend and web image pair from a trusted `main` push,
records their immutable registry digests in a release manifest, and validates
the tuple before a later controller can consider it for deployment.

It does not deploy, contact C00, receive C00 deployment credentials, receive
recovery private keys, or use restored production data. The D2 controller is
the only component allowed to mutate C00.

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
```

The isolated qualification test uses only synthetic, throwaway data. Its
upgrade/rollback command is intentionally disabled until the caller supplies
an admitted tuple and a non-production fixture location.

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
