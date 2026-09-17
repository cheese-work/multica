#!/usr/bin/env bash
# Shared helpers for deploy.sh (single-slot D2 deploy) and cutover.sh (A/B
# slot cutover, CHE-397 unit 2). `source`d, never executed directly — every
# function here is pure plumbing (JSON field reads, image reference parsing,
# digest verification, the migration one-shot runner, tuple capture) with no
# opinion about WHICH services a caller runs it against. deploy.sh calls
# these against the base compose file's `backend`/`frontend` services;
# cutover.sh calls the identical functions against `backend-blue`/
# `backend-green`/etc. from docker-compose.ab.yml — one implementation of
# "how do I safely run the migrator and verify a pulled image", not two that
# can drift apart the way CHE-549's finding 5 happened when a second
# migration-invocation path grew up alongside the first.
#
# Every function assumes the caller has already `cd`'d to the relevant
# compose directory (deploy.sh and cutover.sh both do this immediately after
# arg parsing) and has `compose_dir` set.

# json_field reads one dotted field path out of a JSON file with
# JSON.parse(readFileSync(...)) rather than Node's require() cache: require()
# resolves extensionless files (a GitHub Actions runner temp path, a mktemp
# file with no ".json" suffix) as CommonJS source and throws a SyntaxError on
# a bare JSON document instead of parsing it. JSON.parse never cares about
# the filename.
json_field() {
  node -e '
    const fs = require("node:fs");
    const data = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
    const value = process.argv[2].split(".").reduce((acc, key) => acc?.[key], data);
    process.stdout.write(value === undefined || value === null ? "" : String(value));
  ' "$1" "$2"
}

# image_repo strips the "@sha256:..." suffix off a "repo@sha256:digest"
# reference, leaving just "repo". Splitting on ":" instead would be wrong —
# "repo@sha256:digest" contains a colon *inside* the digest itself, so a
# naive ${ref%%:*} truncates at "repo@sha256" instead of "repo".
image_repo() {
  printf '%s' "${1%%@*}"
}

# image_digest strips everything up to and including "@", leaving
# "sha256:digest" — the counterpart to image_repo.
image_digest() {
  printf '%s' "${1##*@}"
}

# is_valid_digest reports (via exit status) whether its argument is a
# well-formed "sha256:<64 lowercase hex>" digest, and not capture_tuple's
# all-zero placeholder (used when `docker inspect` cannot resolve one; see
# capture-tuple.sh). Callers that compose a "repo@digest" reference for
# rollback must check this first — an unresolvable placeholder digest must
# fall back to a tag reference instead of poisoning the reference.
is_valid_digest() {
  [[ "$1" =~ ^sha256:[0-9a-f]{64}$ ]] || return 1
  [[ "$1" != "sha256:0000000000000000000000000000000000000000000000000000000000000" ]]
}

# bare_repo strips BOTH a possible "@sha256:digest" suffix and a possible
# ":tag" suffix, leaving just the repository. Assumes the registry host has
# no port number (true for every reference this codebase handles — ghcr.io
# never uses one); see deploy.sh's original comment for the port caveat.
bare_repo() {
  local ref=$1
  ref="${ref%%@*}"
  printf '%s' "${ref%%:*}"
}

# verify_pulled_digest confirms the image Docker actually pulled for
# "repo:tag" matches the digest a manifest/packet pinned — the same
# immutability guarantee a raw digest reference would give directly, without
# requiring compose files to accept "repo@digest" syntax (which
# docker-compose.selfhost.yml's hardcoded "${VAR}:${VAR}" templating cannot
# express).
verify_pulled_digest() {
  local repo=$1
  local tag=$2
  local expected_digest=$3
  local actual
  actual="$(docker inspect --format '{{index .RepoDigests 0}}' "${repo}:${tag}" 2>/dev/null | sed -E 's#^.*@##')"
  if [ "$actual" != "$expected_digest" ]; then
    echo "pulled image ${repo}:${tag} digest ${actual:-<none>} does not match manifest digest ${expected_digest}" >&2
    return 1
  fi
}

# compose runs `docker compose -f <files...>` against $compose_dir, with
# whatever -f arguments the caller has set in $compose_files (an array;
# deploy.sh sets it to just docker-compose.selfhost.yml, cutover.sh adds
# deploy/cd/docker-compose.ab.yml on top). Every caller must set
# compose_dir and compose_files before sourcing this file's functions.
compose() {
  local -a f_args=()
  for f in "${compose_files[@]}"; do
    f_args+=(-f "$f")
  done
  (cd "$compose_dir" && docker compose "${f_args[@]}" "$@")
}

# service_published_port asks Compose what host port it actually published
# for the given service's container port, the same way the repo's own
# selfhost installers and `make selfhost` already do (see the port-alias
# comment at the top of docker-compose.selfhost.yml) — resolving the
# BACKEND_PORT/API_PORT/SERVER_PORT/PORT alias chain by hand here would just
# be a second, driftable copy of the same fallback logic.
service_published_port() {
  local service=$1
  local container_port=$2
  compose port "$service" "$container_port" 2>/dev/null | sed -E 's#^.*:##'
}

# wait_ready_on_port polls /readyz on 127.0.0.1:$port until it succeeds or
# timeout_s elapses. Split out from the original wait_ready (which resolved
# its own port via service_published_port) so cutover.sh can wait on a
# specific colour's already-known private port without depending on which
# service name Compose considers "the" backend right now.
wait_ready_on_port() {
  local port=$1
  local timeout_s=$2
  local waited=0
  while [ "$waited" -lt "$timeout_s" ]; do
    if curl --fail --silent --show-error "http://127.0.0.1:${port}/readyz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
    waited=$((waited + 2))
  done
  return 1
}

# wait_ready resolves the given service's published port via Compose, then
# waits on it. This is deploy.sh's original single-slot behavior, kept as a
# thin wrapper over wait_ready_on_port so deploy.sh's own call sites need no
# changes.
wait_ready() {
  local service=$1
  local timeout_s=$2
  local port
  port="$(service_published_port "$service" 8080)"
  if [ -z "$port" ]; then
    echo "could not resolve ${service}'s published port via 'docker compose port'" >&2
    return 1
  fi
  wait_ready_on_port "$port" "$timeout_s"
}

# verify_health_identity confirms /health on the given port reports the
# exact expected commit — the accepted architecture's "require the exact
# expected /health revision" gate. A 200 alone only proves something is
# listening; a stale process serving through an already-open port would
# still pass a bare curl check (see server/cmd/server/health.go's own
# comment on why /health carries commit+pid+started_at).
verify_health_identity() {
  local port=$1
  local expected_commit=$2
  local actual
  actual="$(curl --fail --silent --show-error "http://127.0.0.1:${port}/health" 2>/dev/null | json_field /dev/stdin commit 2>/dev/null)"
  if [ "$actual" != "$expected_commit" ]; then
    echo "/health on port $port reports commit '${actual:-<unreadable>}', expected '$expected_commit'" >&2
    return 1
  fi
}

# capture_tuple records the currently-running stack's identity via
# capture-tuple.sh (see deploy.sh's original comment for why this delegates
# rather than building the JSON inline: an inline version previously emitted
# placeholder digests tuple-snapshot.mjs rejects outright).
capture_tuple() {
  local out=$1
  local application_sha=$2
  bash "$script_dir/capture-tuple.sh" \
    --compose-dir "$compose_dir" \
    --output "$out" \
    --application-sha "$application_sha"
}

# run_migration_step launches exactly one throwaway container from the given
# "repo:tag" backend image, entrypoint overridden to the migrate binary,
# joined to the already-running compose network so it shares DATABASE_URL
# with the rest of the stack. --no-deps means it does not also (re)start
# postgres. Its exit code is the caller's signal for whether it is safe to
# bring up application containers at all.
#
# $migration_service_name (set by the caller) names which compose service's
# image/network the one-shot container should borrow — deploy.sh always
# uses "backend"; cutover.sh uses whichever colour is NOT currently serving,
# so the one-shot container never shares a service name (and therefore never
# risks a `--no-deps` dependency clash) with a colour that is actively
# serving traffic.
run_migration_step() {
  local repo=$1
  local tag=$2
  shift 2
  MULTICA_BACKEND_IMAGE="$repo" \
  MULTICA_IMAGE_TAG="$tag" \
    compose run --rm --no-deps \
    --entrypoint ./migrate \
    "$migration_service_name" "$@"
}

current_ledger_version() {
  compose exec -T postgres psql -U "${POSTGRES_USER:-multica}" -d "${POSTGRES_DB:-multica}" -tA -c \
    "SELECT coalesce(max(version), '') FROM schema_migrations" | tr -d '[:space:]'
}
