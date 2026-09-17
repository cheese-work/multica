#!/usr/bin/env bash
set -euo pipefail

# D2 deploy controller (CHE-530). Runs ON C00 — the GitHub Actions workflow
# (.github/workflows/cd-deploy.yml) copies this script and the admitted
# manifest over SSH and executes it there. It is the only piece of this
# feature that mutates the live self-hosted stack; everything upstream (D1's
# cd-qualification.yml, release-manifest.mjs, admission.mjs) is read-only and
# holds no C00 credentials.
#
# ## Migration ownership (acceptance item 5 — see also deploy/cd/README.md)
#
# Migrations run exactly once per deploy, as a dedicated one-shot step BEFORE
# any new application container starts:
#
#   docker compose run --rm --no-deps -e MULTICA_SKIP_MIGRATIONS= \
#     --entrypoint ./migrate backend up
#
# `docker compose run` starts a throwaway container from the *same backend
# image* being deployed, joined to the same compose network and using the
# same DATABASE_URL, but with its entrypoint overridden straight to the
# migrate binary — it never reaches docker/entrypoint.sh's own migration
# step. Only after this step exits 0 does the script bring up the real
# `backend` / `frontend` services via `docker compose up -d`, and those
# services start with MULTICA_SKIP_MIGRATIONS=1 so entrypoint.sh's own "run
# migrations then exec server" path is skipped — the migration step that
# already ran is not repeated, and a scale-up or restart of the backend
# service can never race a second `migrate up` against the one this script
# already completed. This is provable, not just asserted: the one-shot
# container's exit code gates whether `docker compose up -d` runs at all
# (see run_migration_step and its call site below), and
# MULTICA_SKIP_MIGRATIONS=1 is the literal environment the backend/frontend
# services are started with.
#
# Rollback reverses the same way in one step: if the health check after
# `docker compose up -d` fails, this script restarts the previous image
# tuple and, only if the failed deploy's migrations moved the ledger past
# the previous good version, runs a single bounded
# `migrate down --to <previous-good-version>` one-shot container — built from
# the FAILED (new) backend image, not the previous one. Only that image's
# migrate binary is guaranteed to contain the down-migration files and any
# down-direction hooks/conditions for the versions being reversed; the
# previous image predates them and cannot reverse schema it has never heard
# of (a real deploy hit exactly this: the old binary silently ignored `--to`
# entirely and tore down ~100 unrelated migrations instead of one — see
# CHE-549 findings 5/6). Only the *target version* comes from the previous
# tuple. After the down step, the ledger is re-read and compared against
# that target before anything is reported successful — a rollback that
# leaves the schema short of (or past) the target fails loudly into MANUAL
# INTERVENTION REQUIRED rather than restarting old services against a
# schema they were never built to serve. Never two migration runners
# touching the database at once.
#
# ## What this script does NOT do
#
# It does not build images (D1 already published the qualified pair to
# GHCR), does not decide whether a manifest is admitted (that is
# admission.mjs, run by the calling workflow before this script is invoked),
# and does not hold or read any secret beyond DATABASE-adjacent compose env
# already present in C00's .env — SSH transport and target host are the
# calling workflow's concern, scoped to secrets on cd-deploy.yml only.

usage() {
  cat <<'EOF'
usage: deploy.sh --manifest PATH --compose-dir PATH --state-dir PATH [--dry-run]

  --manifest PATH     A release-candidate manifest (deploy/cd/release-manifest.mjs
                       shape) already verified admitted by the calling workflow.
  --compose-dir PATH  Directory containing docker-compose.selfhost.yml and .env
                       on C00 (the live self-host stack root).
  --state-dir PATH    Directory to read/write the deployed-tuple state file
                       (deployed-tuple.json) and rollback bookkeeping.
  --dry-run           Print the plan and exit 0 without touching Docker.

Exit code is nonzero if the deploy failed AND the automatic rollback also
failed to restore the previous tuple — that combination needs a human.
A failed deploy that rolled back successfully still exits nonzero (the
release did not ship) but the comment/log distinguishes "rolled back
cleanly" from "rollback itself failed".
EOF
}

manifest=""
compose_dir=""
state_dir=""
dry_run=false

while (($#)); do
  case "$1" in
    --manifest) manifest=${2:?}; shift 2 ;;
    --compose-dir) compose_dir=${2:?}; shift 2 ;;
    --state-dir) state_dir=${2:?}; shift 2 ;;
    --dry-run) dry_run=true; shift ;;
    --help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

require() {
  if [ -z "$2" ]; then
    echo "missing $1" >&2
    usage >&2
    exit 2
  fi
}
require --manifest "$manifest"
require --compose-dir "$compose_dir"
require --state-dir "$state_dir"

for path in "$manifest" "$compose_dir/docker-compose.selfhost.yml"; do
  if [ ! -f "$path" ]; then
    echo "required file does not exist: $path" >&2
    exit 1
  fi
done

mkdir -p "$state_dir"
state_file="$state_dir/deployed-tuple.json"
readonly_lock="$state_dir/deploy.lock"

# capture-tuple.sh is a sibling in deploy/cd/ both in the repo and in the
# directory cd-deploy.yml copies to C00 — the workflow ships the pair, not
# deploy.sh alone, precisely so this resolves the same way in both places.
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ ! -f "$script_dir/capture-tuple.sh" ]; then
  echo "capture-tuple.sh is missing from $script_dir — deploy.sh cannot record the deployed tuple without it" >&2
  exit 1
fi

# A second deploy invocation while one is already in flight must not
# interleave migration steps or container restarts with this one. flock
# blocks (rather than failing) so a manually re-triggered workflow run
# queues instead of racing; CI's own per-workflow concurrency group is the
# first line of defense, this is the second for anyone driving deploy.sh
# by hand on the box.
exec 9>"$readonly_lock"
if ! flock -w 600 9; then
  echo "could not acquire deploy lock within 600s — another deploy is running" >&2
  exit 1
fi

# deploy-lib.sh is the shared library both this script and cutover.sh (the
# A/B slot cutover controller, CHE-397 unit 2) source for JSON field reads,
# image reference parsing/verification, the migration one-shot runner and
# tuple capture — one implementation of each, not two that can drift the
# way CHE-549's finding 5 happened when a second migration-invocation path
# grew up independently. See deploy-lib.sh's own header for the full
# rationale.
if [ ! -f "$script_dir/deploy-lib.sh" ]; then
  echo "deploy-lib.sh is missing from $script_dir — deploy.sh cannot run without its shared helpers" >&2
  exit 1
fi
# shellcheck source=deploy-lib.sh
source "$script_dir/deploy-lib.sh"

# compose_files/migration_service_name are deploy-lib.sh's extension points:
# this script's single-slot deploy always targets the base compose file's
# own "backend" service, so both are fixed here once rather than threaded
# through every call site.
compose_files=("docker-compose.selfhost.yml")
migration_service_name="backend"

backend_image="$(json_field "$manifest" images.backend)"
web_image="$(json_field "$manifest" images.web)"
source_sha="$(json_field "$manifest" source_sha)"

if [ ! "$backend_image" ] || [ ! "$web_image" ] || [ ! "$source_sha" ]; then
  echo "manifest is missing images.backend / images.web / source_sha" >&2
  exit 1
fi

# docker-compose.selfhost.yml templates the image reference as a hardcoded
# "${MULTICA_BACKEND_IMAGE:-...}:${MULTICA_IMAGE_TAG:-latest}" — there is a
# literal ":" baked into the compose file between the two variables, so it
# is structurally impossible to inject a "repo@sha256:digest" reference
# through them (that would produce "repo@sha256:digest:latest" or similar
# garbage). Deploy by the same "sha-<commit>" tag D1's cd-qualification.yml
# already publishes for every image it builds, then verify the digest that
# tag resolves to on this host matches the manifest's pinned digest before
# trusting it — tag for compose compatibility, digest check for the same
# immutability guarantee a raw digest reference would have given directly.
backend_repo="$(image_repo "$backend_image")"
backend_digest="$(image_digest "$backend_image")"
web_repo="$(image_repo "$web_image")"
web_digest="$(image_digest "$web_image")"
image_tag="sha-${source_sha}"

echo "==> deploy plan"
echo "    source_sha:    $source_sha"
echo "    backend_image: $backend_image"
echo "    web_image:     $web_image"
echo "    compose_dir:   $compose_dir"
echo "    state_file:    $state_file"

if [ "$dry_run" = true ]; then
  echo "--dry-run: not touching Docker"
  exit 0
fi

previous_tuple=""
if [ -f "$state_file" ]; then
  previous_tuple="$state_dir/previous-tuple.json"
  cp "$state_file" "$previous_tuple"
  echo "==> previous deployed tuple recorded at $previous_tuple"
else
  echo "==> no previous deployed-tuple.json found; treating this as the first D2-managed deploy (no rollback baseline)"
fi

previous_backend_image=""
previous_web_image=""
previous_good_version=""
if [ -n "$previous_tuple" ]; then
  previous_backend_ref="$(json_field "$previous_tuple" images.backend.reference)"
  previous_backend_digest="$(json_field "$previous_tuple" images.backend.digest)"
  previous_web_ref="$(json_field "$previous_tuple" images.web.reference)"
  previous_web_digest="$(json_field "$previous_tuple" images.web.digest)"
  previous_good_version="$(json_field "$previous_tuple" migration_ledger.latest.version)"

  # capture_tuple can have recorded a placeholder all-zero digest when
  # `docker inspect` could not resolve a real RepoDigest at capture time (see
  # capture_tuple's own comment). Composing "repo@sha256:0000...0000" here
  # would produce a reference that can never resolve, poisoning rollback
  # exactly when it is needed most. Fall back to the tag-based "repo:tag"
  # reference (previous_backend_ref / previous_web_ref are already in that
  # form — see capture_tuple) instead of silently proceeding with an
  # unresolvable digest reference.
  if is_valid_digest "$previous_backend_digest"; then
    previous_backend_image="$(bare_repo "$previous_backend_ref")@${previous_backend_digest}"
  else
    echo "==> previous backend digest is missing or a placeholder; falling back to tag reference $previous_backend_ref for rollback" >&2
    previous_backend_image="$previous_backend_ref"
  fi
  if is_valid_digest "$previous_web_digest"; then
    previous_web_image="$(bare_repo "$previous_web_ref")@${previous_web_digest}"
  else
    echo "==> previous web digest is missing or a placeholder; falling back to tag reference $previous_web_ref for rollback" >&2
    previous_web_image="$previous_web_ref"
  fi
  echo "==> previous good migration version: ${previous_good_version:-<unknown>}"
fi

rollback() {
  local reason=$1
  echo "!! deploy failed: $reason" >&2
  if [ -z "$previous_backend_image" ]; then
    echo "!! no previous tuple to roll back to; leaving the failed stack as-is for manual triage" >&2
    exit 1
  fi

  echo "==> rolling back to previous tuple"
  echo "    previous_backend_image: $previous_backend_image"
  echo "    previous_web_image:     $previous_web_image"

  # Stop the failed new containers before touching the schema, so nothing
  # holds connections against a database mid-rollback.
  compose stop backend frontend >/dev/null 2>&1 || true

  # previous_good_tag reuses whatever tag the previous tuple's application
  # SHA implies (D1's "sha-<commit>" convention) — the same convention this
  # script uses for the forward path below.
  previous_good_tag="sha-$(json_field "$previous_tuple" application_sha)"

  if [ -n "$previous_good_version" ]; then
    current_version="$(compose exec -T postgres psql -U "${POSTGRES_USER:-multica}" -d "${POSTGRES_DB:-multica}" -tA -c \
      "SELECT coalesce(max(version), '') FROM schema_migrations" | tr -d '[:space:]')"
    if [ "$current_version" != "$previous_good_version" ]; then
      echo "==> failed deploy's migrations moved the ledger past the previous good version; rolling back schema to $previous_good_version"
      # Use the FAILED (new) backend image's migrate binary for the down
      # migrations, not the previous one: it is the binary that shipped
      # alongside these down-migration files and any down-direction
      # hooks/conditions they need. The previous image predates the
      # versions being reversed and cannot know how to reverse schema it
      # has never heard of — an older `migrate` binary silently ignores an
      # unrecognized `--to` flag and falls back to a full unbounded `down`,
      # tearing down every migration back to 001 instead of stopping at the
      # target (CHE-549 finding 5). Only the target version comes from the
      # previous tuple; the image and migration files come from the image
      # actually being rolled back.
      if ! run_migration_step "$backend_repo" "$image_tag" down --to "$previous_good_version"; then
        echo "!! bounded rollback migration failed — database schema is in an indeterminate state between $current_version and $previous_good_version" >&2
        echo "!! MANUAL INTERVENTION REQUIRED before restarting any application container" >&2
        exit 1
      fi

      # A down step that exits 0 is not sufficient evidence of a correct
      # rollback: if the down-migration files needed to reach
      # previous_good_version are missing or no-op against this schema, the
      # migrate binary can still exit 0 having reversed nothing (CHE-549
      # finding 6). Re-read the ledger and refuse to proceed unless it
      # landed exactly on target — reporting success with the schema ahead
      # of (or behind) where the restarted old services expect it is worse
      # than failing loudly here.
      post_rollback_version="$(compose exec -T postgres psql -U "${POSTGRES_USER:-multica}" -d "${POSTGRES_DB:-multica}" -tA -c \
        "SELECT coalesce(max(version), '') FROM schema_migrations" | tr -d '[:space:]')"
      if [ "$post_rollback_version" != "$previous_good_version" ]; then
        echo "!! bounded rollback reported success but ledger is at '$post_rollback_version', not the target '$previous_good_version' — database schema is in an indeterminate state" >&2
        echo "!! MANUAL INTERVENTION REQUIRED before restarting any application container" >&2
        exit 1
      fi
    else
      echo "==> ledger already at previous good version $previous_good_version; no schema rollback needed"
    fi
  else
    echo "==> previous tuple has no recorded migration version; skipping schema rollback (assuming no migrations ran since it was deployed)"
  fi

  # Guarded (not a bare statement): under `set -euo pipefail`, an unguarded
  # failure here would abort the script on the spot — skipping wait_ready
  # below and skipping the "MANUAL INTERVENTION REQUIRED" diagnostic this
  # function's other failure paths already give the operator. A failure to
  # even launch the rollback restart is exactly the kind of failure that
  # diagnostic exists for.
  if ! MULTICA_BACKEND_IMAGE="$(bare_repo "$previous_backend_image")" \
    MULTICA_WEB_IMAGE="$(bare_repo "$previous_web_image")" \
    MULTICA_IMAGE_TAG="$previous_good_tag" \
    compose up -d --no-deps backend frontend; then
    echo "!! rollback restart failed to launch — MANUAL INTERVENTION REQUIRED" >&2
    exit 1
  fi

  if wait_ready backend 120; then
    echo "==> rollback complete: previous tuple restored and healthy"
    exit 1 # deploy still failed overall — this is a successful rollback of a failed deploy
  fi
  echo "!! rollback restart did not become healthy within 120s — MANUAL INTERVENTION REQUIRED" >&2
  exit 1
}

echo "==> pulling qualified image pair (tag ${image_tag})"
MULTICA_BACKEND_IMAGE="$backend_repo" \
MULTICA_WEB_IMAGE="$web_repo" \
MULTICA_IMAGE_TAG="$image_tag" \
  compose pull backend frontend || rollback "image pull failed"

echo "==> verifying pulled images match the admitted manifest's digests"
if ! verify_pulled_digest "$backend_repo" "$image_tag" "$backend_digest" \
  || ! verify_pulled_digest "$web_repo" "$image_tag" "$web_digest"; then
  rollback "pulled image digest did not match the admitted manifest"
fi

echo "==> running migration step (one-shot, before any new container starts)"
if ! run_migration_step "$backend_repo" "$image_tag" up; then
  rollback "migration step failed"
fi

echo "==> starting new containers (MULTICA_SKIP_MIGRATIONS=1 — migrations already applied above)"
MULTICA_BACKEND_IMAGE="$backend_repo" \
MULTICA_WEB_IMAGE="$web_repo" \
MULTICA_IMAGE_TAG="$image_tag" \
MULTICA_SKIP_MIGRATIONS=1 \
  compose up -d --no-deps backend frontend || rollback "container start failed"

echo "==> health-checking new deployment"
if ! wait_ready backend 180; then
  rollback "new deployment did not become ready within 180s"
fi

echo "==> deploy succeeded; recording deployed tuple"
capture_tuple "$state_file" "$source_sha"
cat "$state_file"

echo "==> deploy of $source_sha complete"
