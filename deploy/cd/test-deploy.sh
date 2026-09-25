#!/usr/bin/env bash
set -euo pipefail

# Behavioral control-flow test for deploy.sh (CHE-530): pull -> digest-verify
# -> one-shot migrate -> `up -d` -> health-check -> rollback-on-failure. This
# is the test deploy/cd/README.md's "What is NOT verified" section referred
# to as missing; it now exists.
#
# deploy.sh talks to `docker`, `docker compose`, `curl`, and `sleep`. None of
# those are real here — this script builds a scripted mock bin/ directory,
# puts it first on PATH, and drives each mock's behavior (pull ok/fail,
# migrate ok/fail, container-start ok/fail, health-check ok/never) through
# small control files a fresh scenario writes before invoking deploy.sh.
# `sleep` is mocked too, purely so the "health check never passes" scenario
# does not burn a real 120-180s wall-clock wait_ready timeout — deploy.sh's
# own timeouts are untouched.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

mock_bin="$work_dir/bin"
mkdir -p "$mock_bin"

source_sha="504078f8ea7fa31f342f195659e93a7f6c3e5a91"
backend_digest="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
web_digest="sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
previous_sha="393e6ecb0a5b6bb90c3ec5e7d2e6b1f4c9a2d3e0"
previous_backend_digest="sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
previous_web_digest="sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
placeholder_digest="sha256:0000000000000000000000000000000000000000000000000000000000000"

# ---------------------------------------------------------------------------
# Mock bin/: docker, curl, sleep. Control files under $work_dir/control tell
# them how to behave; deploy.sh itself is never touched.
# ---------------------------------------------------------------------------

cat >"$mock_bin/docker" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail

control_dir="${DEPLOY_TEST_CONTROL_DIR:?DEPLOY_TEST_CONTROL_DIR unset}"
log_file="$control_dir/docker-calls.log"
printf '%s\n' "$*" >>"$log_file"

fail_if_flagged() {
  local flag=$1
  if [ -f "$control_dir/$flag" ]; then
    echo "mock docker: forced failure ($flag)" >&2
    exit 1
  fi
}

if [ "$1" = "inspect" ]; then
  # deploy.sh and capture-tuple.sh ask `docker inspect` four different
  # questions, distinguished by their --format string:
  #   {{index .RepoDigests 0}}        -> the registry digest (digest verify)
  #   {{.Id}}                         -> the local image id (digest fallback)
  #   ...image.revision label         -> the commit the image was built from
  #   ...compose.config-hash label    -> Compose's own service config hash
  fmt=""
  prev_arg=""
  for a in "$@"; do
    case "$prev_arg" in
      --format) fmt="$a" ;;
    esac
    prev_arg="$a"
  done
  ref="${*: -1}"
  repo="${ref%%:*}"
  digest=""
  case "$repo" in
    *multica-backend) digest="${MOCK_BACKEND_DIGEST:-}" ;;
    *multica-web) digest="${MOCK_WEB_DIGEST:-}" ;;
  esac

  case "$fmt" in
    *config-hash*)
      # A real 64-character Compose config hash; capture-tuple.sh redacts it
      # to the 8-char prefix / 4-char suffix the tuple schema requires.
      printf '%s\n' "aaaabbbbccccddddeeeeffff00001111222233334444555566667777888899990"
      ;;
    *image.revision*)
      printf '%s\n' "${MOCK_IMAGE_REVISION:-}"
      ;;
    *.Id*)
      if [ -n "$digest" ]; then printf '%s\n' "$digest"; fi
      ;;
    *)
      if [ -n "$digest" ] && [ -f "$control_dir/inspect-resolves" ]; then
        printf '%s@%s\n' "$repo" "$digest"
      fi
      ;;
  esac
  exit 0
fi

# known_services lists the real docker-compose.selfhost.yml service names
# (postgres, backend, frontend — verified against `docker compose config
# --services` on both C00 and this branch's own compose file). Any argument
# that looks like a bare service name (no leading "-", not a flag value we
# already consumed) must be one of these; a future rename back to the wrong
# name (e.g. "web", which is NOT a compose service — it is only the image
# name / manifest JSON key) must fail this mock the same way real Compose
# fails with "no such service: web", so the regression cannot silently come
# back.
known_services=(postgres backend frontend)
is_known_service() {
  local svc=$1 s
  for s in "${known_services[@]}"; do
    [ "$svc" = "$s" ] && return 0
  done
  return 1
}
check_service_args() {
  # Validates only bare trailing positional args (the service name list at
  # the end of pull/up/stop), not flags or their values.
  local a
  for a in "$@"; do
    case "$a" in
      -*) continue ;;
    esac
    if ! is_known_service "$a"; then
      echo "mock docker compose: no such service: $a" >&2
      exit 1
    fi
  done
}

if [ "$1" = "login" ]; then
  # login ghcr.io -u token --password-stdin — deploy.sh's GHCR_PULL_TOKEN
  # gate (CHE-549). Consume stdin so the real password-stdin protocol is
  # exercised. Check the forced-failure flag BEFORE logging the password: a
  # real failed login is exactly the case where the caller must not be able
  # to prove the registry ever saw the token, so a passing "login failed"
  # scenario must not find its password in this log either.
  password="$(cat)"
  if [ -f "$control_dir/fail-ghcr-login" ]; then
    printf 'login %s <rejected>\n' "$2" >>"$control_dir/registry-auth.log"
    echo "mock docker: forced failure (fail-ghcr-login)" >&2
    exit 1
  fi
  printf 'login %s password=%s\n' "$2" "$password" >>"$control_dir/registry-auth.log"
  exit 0
fi

if [ "$1" = "logout" ]; then
  printf 'logout %s\n' "$2" >>"$control_dir/registry-auth.log"
  exit 0
fi

if [ "$1" = "compose" ]; then
  shift
  # shift off "-f docker-compose.selfhost.yml"
  if [ "$1" = "-f" ]; then shift 2; fi
  sub="$1"; shift
  case "$sub" in
    pull)
      check_service_args "$@"
      fail_if_flagged fail-pull
      exit 0
      ;;
    run)
      # run --rm --no-deps --entrypoint ./migrate backend up|down --to <v>
      # MULTICA_BACKEND_IMAGE / MULTICA_IMAGE_TAG are exported by
      # run_migration_step's caller (deploy.sh), not passed as argv, so log
      # them separately — this is what lets a test assert *which* image the
      # down-migration ran against (CHE-549 finding 5 regression coverage).
      args=("$@")
      direction=""
      to_target=""
      next_is_to=""
      for a in "${args[@]}"; do
        if [ -n "$next_is_to" ]; then
          to_target="$a"
          next_is_to=""
          continue
        fi
        case "$a" in
          up) direction="up" ;;
          down) direction="down" ;;
          --to) next_is_to=1 ;;
        esac
      done
      printf 'migrate %s image=%s tag=%s to=%s\n' \
        "$direction" "${MULTICA_BACKEND_IMAGE:-}" "${MULTICA_IMAGE_TAG:-}" "$to_target" \
        >>"$control_dir/migrate-calls.log"
      if [ "$direction" = "down" ]; then
        fail_if_flagged fail-migrate-down
        if [ -f "$control_dir/migrate-down-noop" ]; then
          # Simulates CHE-549 finding 6: the down step exits 0 having
          # reversed nothing (e.g. the target image lacks the needed
          # down-migration files), so the ledger stays wherever it already
          # was instead of landing on the --to target.
          exit 0
        fi
        if [ -f "$control_dir/ledger-version-after-rollback" ]; then
          cp "$control_dir/ledger-version-after-rollback" "$control_dir/ledger-version"
        fi
      else
        fail_if_flagged fail-migrate-up
      fi
      exit 0
      ;;
    up)
      # up -d --no-deps backend frontend — used both for the forward deploy
      # and for rollback's restart of the previous tuple. Distinguish which
      # one via separate flags so a test can fail just one of the two.
      check_service_args "$@"
      if [ -f "$control_dir/rollback-in-progress" ]; then
        fail_if_flagged fail-rollback-restart
      else
        fail_if_flagged fail-container-start
      fi
      touch "$control_dir/containers-up"
      exit 0
      ;;
    stop)
      check_service_args "$@"
      touch "$control_dir/rollback-in-progress"
      exit 0
      ;;
    port)
      echo "0.0.0.0:18080"
      exit 0
      ;;
    exec)
      # exec -T postgres psql ... -c "SELECT ..."
      if [ -f "$control_dir/ledger-version" ]; then
        version="$(cat "$control_dir/ledger-version")"
      else
        version="467_autopilot_trigger_creator_from_autopilot"
      fi
      # deploy.sh has two distinct query shapes against this same mock:
      # capture_tuple's "count|version|applied_at" query (-F'|', three
      # columns) and the rollback path's bare "coalesce(max(version), '')"
      # query (-tA, single column, no pipe). Emitting the three-column shape
      # unconditionally made every rollback-path version comparison compare
      # against "1|<version>|<timestamp>", which never equals a bare version
      # string — silently forcing the schema-rollback branch to fire on
      # every scenario regardless of the fixture's actual ledger state.
      # Detect which query this call is by its final "-c <sql>" argument.
      sql="${*: -1}"
      # Order matters: capture-tuple.sh's summary query contains BOTH
      # "count(*)" and "coalesce(max(version)", so the count case must be
      # tested first or the single-column branch swallows it and returns a
      # bare version where three fields were expected.
      case "$sql" in
        *"count(*)"*)
          # capture-tuple.sh's three-column summary: count|version|applied_at,
          # with applied_at already formatted RFC3339 by to_char in the
          # query itself. row_count must be a positive integer or
          # tuple-snapshot.mjs rejects the tuple.
          printf '%s|%s|%s\n' "${MOCK_LEDGER_COUNT:-529}" "$version" "2026-09-12T08:58:23Z"
          ;;
        *"ORDER BY version"*)
          # capture-tuple.sh's ordered-ledger query: it hashes this output
          # into migration_ledger.ordered_sha256, so any stable set of
          # version rows is a valid answer.
          printf '%s\n' "$version"
          ;;
        *"coalesce(max(version)"*)
          printf '%s\n' "$version"
          ;;
        *)
          printf '1|%s|2026-09-12T08:58:23.324992Z\n' "$version"
          ;;
      esac
      exit 0
      ;;
    images)
      # images <service> --format json. capture-tuple.sh (which deploy.sh
      # delegates its tuple recording to) resolves the RUNNING service's
      # image reference from this, so it must answer with the tuple the
      # scenario is deploying rather than an empty list — a real host always
      # has a running service here by the time the tuple is captured.
      svc="${1:-backend}"
      case "$svc" in
        backend) printf '[{"Repository":"ghcr.io/cheese-work/multica-backend","Tag":"%s"}]\n' "${MOCK_IMAGE_TAG:-latest}" ;;
        frontend) printf '[{"Repository":"ghcr.io/cheese-work/multica-web","Tag":"%s"}]\n' "${MOCK_IMAGE_TAG:-latest}" ;;
        *) printf '[]\n' ;;
      esac
      exit 0
      ;;
    ps)
      # ps -q <service> — capture-tuple.sh reads the container id to fetch
      # Compose's own config-hash label for that service.
      printf 'mock-container-%s\n' "${*: -1}"
      exit 0
      ;;
    *)
      echo "mock docker compose: unhandled subcommand $sub" >&2
      exit 1
      ;;
  esac
fi

echo "mock docker: unhandled command $*" >&2
exit 1
MOCK
chmod +x "$mock_bin/docker"

cat >"$mock_bin/curl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
control_dir="${DEPLOY_TEST_CONTROL_DIR:?DEPLOY_TEST_CONTROL_DIR unset}"
if [ -f "$control_dir/health-never-ready" ]; then
  exit 22
fi
if [ -f "$control_dir/containers-up" ]; then
  exit 0
fi
exit 22
MOCK
chmod +x "$mock_bin/curl"

# sleep is mocked purely to make wait_ready's real 2s-per-iteration polling
# loop finish instantly in the "health check never passes" scenario; deploy.sh's
# own timeout math (120s / 180s budgets, waited += 2) is exercised unchanged,
# it just no longer costs 60-90 real iterations of wall-clock time.
cat >"$mock_bin/sleep" <<'MOCK'
#!/usr/bin/env bash
exit 0
MOCK
chmod +x "$mock_bin/sleep"

export PATH="$mock_bin:$PATH"

# ---------------------------------------------------------------------------
# Fixtures shared by every scenario.
# ---------------------------------------------------------------------------

compose_dir="$work_dir/compose"
mkdir -p "$compose_dir"
touch "$compose_dir/docker-compose.selfhost.yml"

manifest="$work_dir/manifest.json"
cat >"$manifest" <<JSON
{
  "images": {
    "backend": "ghcr.io/cheese-work/multica-backend@$backend_digest",
    "web": "ghcr.io/cheese-work/multica-web@$web_digest"
  },
  "source_sha": "$source_sha"
}
JSON

previous_tuple_fixture="$work_dir/previous-tuple.json"
cat >"$previous_tuple_fixture" <<JSON
{
  "schema_version": 1,
  "role": "c00-deployment-baseline-read-only-metadata",
  "captured_at": "2026-09-01T00:00:00Z",
  "application_sha": "$previous_sha",
  "images": {
    "backend": { "reference": "ghcr.io/cheese-work/multica-backend:sha-$previous_sha", "digest": "$previous_backend_digest" },
    "web": { "reference": "ghcr.io/cheese-work/multica-web:sha-$previous_sha", "digest": "$previous_web_digest" }
  },
  "compose": {
    "file": "docker-compose.selfhost.yml",
    "sha256": "$placeholder_digest",
    "service_config_sha256": { "backend": { "prefix": "00000000", "suffix": "0000" }, "web": { "prefix": "00000000", "suffix": "0000" } }
  },
  "migration_ledger": {
    "row_count": 10,
    "latest": { "version": "440_seed", "applied_at": "2026-09-01T00:00:00Z" },
    "ordered_sha256": "$placeholder_digest"
  }
}
JSON

run_deploy() {
  local state_dir=$1
  mkdir -p "$state_dir"
  export DEPLOY_TEST_CONTROL_DIR="$state_dir/control"
  mkdir -p "$DEPLOY_TEST_CONTROL_DIR"
  # docker inspect is only ever called for the NEW (forward) backend/web
  # repo:tag being deployed (verify_pulled_digest, capture_tuple) — never
  # for the previous tuple during rollback — so these two always resolve to
  # the manifest's own digests regardless of scenario.
  export MOCK_BACKEND_DIGEST="$backend_digest"
  export MOCK_WEB_DIGEST="$web_digest"
  bash deploy/cd/deploy.sh \
    --manifest "$manifest" \
    --compose-dir "$compose_dir" \
    --state-dir "$state_dir"
}

fresh_scenario_dir() {
  local name=$1
  local dir="$work_dir/scenarios/$name"
  rm -rf "$dir"
  mkdir -p "$dir"
  printf '%s\n' "$dir"
}

expect_exit() {
  local want=$1
  local got=$2
  local name=$3
  if [ "$got" -ne "$want" ]; then
    echo "scenario $name: exit $got, want $want" >&2
    exit 1
  fi
}

expect_contains() {
  local haystack=$1
  local needle=$2
  local name=$3
  if [[ "$haystack" != *"$needle"* ]]; then
    echo "scenario $name: output missing expected text: $needle" >&2
    echo "--- captured output ---" >&2
    echo "$haystack" >&2
    exit 1
  fi
}

expect_not_contains() {
  local haystack=$1
  local needle=$2
  local name=$3
  if [[ "$haystack" == *"$needle"* ]]; then
    echo "scenario $name: output unexpectedly contains: $needle" >&2
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# Scenario 1: happy path — no previous tuple, pull/migrate/start/health all
# succeed. deploy.sh must exit 0 and record a deployed-tuple.json.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir happy-path)"
control_dir="$state_dir/control"
mkdir -p "$control_dir"
touch "$control_dir/inspect-resolves"
output="$(run_deploy "$state_dir" 2>&1)"
status=$?
expect_exit 0 "$status" happy-path
expect_contains "$output" "deploy succeeded" happy-path
if [ ! -f "$state_dir/deployed-tuple.json" ]; then
  echo "scenario happy-path: deployed-tuple.json was not written" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario 2: pull fails -> rollback. No previous tuple exists, so rollback
# has nothing to restore and must exit nonzero without attempting a restart.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir pull-fails)"
control_dir="$state_dir/control"
mkdir -p "$control_dir"
touch "$control_dir/fail-pull"
set +e
output="$(run_deploy "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" pull-fails
expect_contains "$output" "image pull failed" pull-fails
expect_contains "$output" "no previous tuple to roll back to" pull-fails

# ---------------------------------------------------------------------------
# Scenario 3: migrate fails -> rollback restores the previous tuple and
# reports a clean rollback of a failed deploy (still nonzero overall).
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir migrate-fails)"
mkdir -p "$state_dir"
cp "$previous_tuple_fixture" "$state_dir/deployed-tuple.json"
control_dir="$state_dir/control"
mkdir -p "$control_dir"
touch "$control_dir/inspect-resolves" "$control_dir/fail-migrate-up"
# The default pre-rollback ledger (467_...) sits past this fixture's
# previous_good_version (440_seed), so the bounded down-migration fires;
# tell the mock it actually lands on target, matching a real bounded
# rollback (finding 5/6 regressions get their own dedicated scenarios below).
printf '440_seed\n' >"$control_dir/ledger-version-after-rollback"
set +e
output="$(run_deploy "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" migrate-fails
expect_contains "$output" "migration step failed" migrate-fails
expect_contains "$output" "rolling back to previous tuple" migrate-fails
expect_contains "$output" "rollback complete: previous tuple restored and healthy" migrate-fails

# ---------------------------------------------------------------------------
# Scenario 4: health check never passes -> rollback restores the previous
# tuple successfully.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir health-check-never-passes)"
mkdir -p "$state_dir"
cp "$previous_tuple_fixture" "$state_dir/deployed-tuple.json"
control_dir="$state_dir/control"
mkdir -p "$control_dir"
touch "$control_dir/inspect-resolves" "$control_dir/health-never-ready"
printf '440_seed\n' >"$control_dir/ledger-version-after-rollback"
set +e
output="$(run_deploy "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" health-check-never-passes
expect_contains "$output" "did not become ready within 180s" health-check-never-passes
expect_contains "$output" "rolling back to previous tuple" health-check-never-passes
# The rollback restart's own health check is also mocked to never pass, so
# rollback itself should report failure, not false success.
expect_contains "$output" "rollback restart did not become healthy within 120s" health-check-never-passes
expect_contains "$output" "MANUAL INTERVENTION REQUIRED" health-check-never-passes

# ---------------------------------------------------------------------------
# Scenario 5 (regression for Task 1): rollback restart itself fails to
# launch. This is the bare-statement-under-set-e bug: deploy.sh must not
# abort silently — it must still print the "MANUAL INTERVENTION REQUIRED"
# diagnostic and exit nonzero, never skip straight past wait_ready's
# companion diagnostics.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir rollback-restart-fails)"
mkdir -p "$state_dir"
cp "$previous_tuple_fixture" "$state_dir/deployed-tuple.json"
control_dir="$state_dir/control"
mkdir -p "$control_dir"
touch "$control_dir/inspect-resolves" "$control_dir/fail-container-start" "$control_dir/fail-rollback-restart"
printf '440_seed\n' >"$control_dir/ledger-version-after-rollback"
set +e
output="$(run_deploy "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" rollback-restart-fails
expect_contains "$output" "container start failed" rollback-restart-fails
expect_contains "$output" "rollback restart failed to launch" rollback-restart-fails
expect_contains "$output" "MANUAL INTERVENTION REQUIRED" rollback-restart-fails
# Must not claim a healthy rollback when the restart never launched.
expect_not_contains "$output" "rollback complete: previous tuple restored and healthy" rollback-restart-fails

# ---------------------------------------------------------------------------
# Scenario 6 (regression for Task 2): the previous tuple's recorded digest is
# the capture_tuple placeholder. Rollback must fall back to the tag
# reference instead of composing an unresolvable "repo@sha256:0000...0000"
# reference, and must say so.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir placeholder-digest-fallback)"
mkdir -p "$state_dir"
node -e '
  const fs = require("node:fs");
  const t = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
  t.images.backend.digest = process.argv[3];
  t.images.web.digest = process.argv[3];
  fs.writeFileSync(process.argv[2], JSON.stringify(t));
' "$previous_tuple_fixture" "$state_dir/deployed-tuple.json" "$placeholder_digest"
control_dir="$state_dir/control"
mkdir -p "$control_dir"
touch "$control_dir/inspect-resolves" "$control_dir/fail-migrate-up"
printf '440_seed\n' >"$control_dir/ledger-version-after-rollback"
set +e
output="$(run_deploy "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" placeholder-digest-fallback
expect_contains "$output" "falling back to tag reference" placeholder-digest-fallback
expect_not_contains "$output" "@$placeholder_digest" placeholder-digest-fallback

# ---------------------------------------------------------------------------
# Scenario 7 (regression for Defect 1 — CHE-530 live C00 inspection): the
# real compose service is "frontend", not "web". "web" is legitimately the
# image name (multica-web) and the manifest/tuple JSON key, but it is not a
# compose service — `docker compose pull backend web` fails with "no such
# service: web" on the real stack. Simulate a regression back to the wrong
# service name and confirm the mock (and therefore a real Compose) rejects
# it, so deploy.sh's actual service-name arguments are what is under test,
# not just its control flow.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir wrong-service-name-rejected)"
control_dir="$state_dir/control"
mkdir -p "$control_dir"
touch "$control_dir/inspect-resolves"
set +e
output="$(env DEPLOY_TEST_CONTROL_DIR="$control_dir" "$mock_bin/docker" compose -f docker-compose.selfhost.yml pull backend web 2>&1)"
status=$?
set -e
expect_exit 1 "$status" wrong-service-name-rejected
expect_contains "$output" "no such service: web" wrong-service-name-rejected

# Confirm deploy.sh itself only ever asks Compose for real service names —
# i.e. that Defect 1's fix actually landed in deploy.sh, not just in this
# mock's allowlist.
if grep -qE '\b(pull|stop)\s+backend\s+web\b|up\s+-d\s+--no-deps\s+backend\s+web\b' "$root_dir/deploy/cd/deploy.sh"; then
  echo "scenario wrong-service-name-rejected: deploy.sh still invokes compose with the nonexistent 'web' service" >&2
  exit 1
fi
if ! grep -qE '\b(pull|stop)\s+backend\s+frontend\b|up\s+-d\s+--no-deps\s+backend\s+frontend\b' "$root_dir/deploy/cd/deploy.sh"; then
  echo "scenario wrong-service-name-rejected: deploy.sh does not invoke compose with the real 'frontend' service" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario 8 (regression for Defect 2 — CHE-530 live C00 inspection):
# MULTICA_SKIP_MIGRATIONS must be declared in the backend service's
# environment block of docker-compose.selfhost.yml, or Compose never
# forwards deploy.sh's ambient export into the container and the guard in
# docker/entrypoint.sh silently does nothing. Prefer a real
# `docker compose config` render when docker is available (proves the
# variable actually resolves end to end); fall back to a pure-bash grep
# scoped to the backend service block otherwise so this test still runs
# without docker.
# ---------------------------------------------------------------------------
compose_file="$root_dir/docker-compose.selfhost.yml"
if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
  rendered="$(JWT_SECRET=test-jwt-secret-not-a-real-secret-0000 MULTICA_SKIP_MIGRATIONS=1 \
    docker compose -f "$compose_file" config 2>/dev/null)"
  if ! printf '%s' "$rendered" | grep -qE '^\s*MULTICA_SKIP_MIGRATIONS:\s*"1"\s*$'; then
    echo "scenario skip-migrations-declared: docker compose config did not forward MULTICA_SKIP_MIGRATIONS=1 into the rendered config" >&2
    exit 1
  fi
else
  # Pure-bash fallback: extract the backend service's block (from "  backend:"
  # up to the next top-level "  <name>:" service key) and grep within it,
  # so a MULTICA_SKIP_MIGRATIONS line elsewhere in the file (e.g. a comment)
  # cannot produce a false pass.
  backend_block="$(awk '
    /^  backend:/ { capture=1 }
    capture && /^  [a-zA-Z_-]+:/ && !/^  backend:/ { capture=0 }
    capture { print }
  ' "$compose_file")"
  if ! printf '%s' "$backend_block" | grep -qE 'MULTICA_SKIP_MIGRATIONS:\s*\$\{MULTICA_SKIP_MIGRATIONS:-'; then
    echo "scenario skip-migrations-declared: backend service environment block does not declare MULTICA_SKIP_MIGRATIONS" >&2
    exit 1
  fi
fi

# ---------------------------------------------------------------------------
# Scenario 9 (regression for CHE-549 finding 5): the bounded down-migration
# must run against the FAILED (new) backend image's repo/tag, never the
# previous tuple's — an older migrate binary silently ignores an unknown
# --to flag and tears down everything back to 001. Force the ledger to sit
# past the previous tuple's recorded version so the rollback path's schema
# branch actually fires, then assert the migrate-calls.log entry for the
# down step names the new image (backend_digest-qualified manifest image,
# tagged sha-<source_sha>) and the previous tuple's target version — never
# the previous tuple's own image reference.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir rollback-uses-failed-image-for-down)"
mkdir -p "$state_dir"
cp "$previous_tuple_fixture" "$state_dir/deployed-tuple.json"
control_dir="$state_dir/control"
mkdir -p "$control_dir"
touch "$control_dir/inspect-resolves"
# capture_tuple never runs on this failure path, so this is the ledger the
# rollback's own pre-check reads: ahead of the fixture's previous_good_version
# (440_seed), which is what makes the down step actually fire.
printf '467_autopilot_trigger_creator_from_autopilot\n' >"$control_dir/ledger-version"
printf '440_seed\n' >"$control_dir/ledger-version-after-rollback"
touch "$control_dir/fail-container-start"
set +e
output="$(run_deploy "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" rollback-uses-failed-image-for-down
expect_contains "$output" "rolling back schema to 440_seed" rollback-uses-failed-image-for-down
migrate_log="$(cat "$control_dir/migrate-calls.log")"
expect_contains "$migrate_log" "migrate down image=ghcr.io/cheese-work/multica-backend tag=sha-$source_sha to=440_seed" rollback-uses-failed-image-for-down
expect_not_contains "$migrate_log" "image=ghcr.io/cheese-work/multica-backend tag=sha-$previous_sha" rollback-uses-failed-image-for-down

# ---------------------------------------------------------------------------
# Scenario 10 (regression for CHE-549 finding 6): the down step can exit 0
# having reversed nothing (missing/no-op down-migrations against the new
# image). deploy.sh must re-read the ledger after the down step and refuse
# to report a successful rollback unless it actually landed on the target —
# never restart old services against a schema still ahead of what they
# expect, and never write a deployed-tuple.json recording a version that was
# never true.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir rollback-down-noop-fails-loud)"
mkdir -p "$state_dir"
cp "$previous_tuple_fixture" "$state_dir/deployed-tuple.json"
control_dir="$state_dir/control"
mkdir -p "$control_dir"
touch "$control_dir/inspect-resolves"
printf '467_autopilot_trigger_creator_from_autopilot\n' >"$control_dir/ledger-version"
touch "$control_dir/fail-container-start" "$control_dir/migrate-down-noop"
set +e
output="$(run_deploy "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" rollback-down-noop-fails-loud
expect_contains "$output" "bounded rollback reported success but ledger is at '467_autopilot_trigger_creator_from_autopilot', not the target '440_seed'" rollback-down-noop-fails-loud
expect_contains "$output" "MANUAL INTERVENTION REQUIRED" rollback-down-noop-fails-loud
expect_not_contains "$output" "rollback complete: previous tuple restored and healthy" rollback-down-noop-fails-loud
if [ -f "$state_dir/deployed-tuple.json.new" ]; then
  echo "scenario rollback-down-noop-fails-loud: deployed-tuple.json was updated despite the failed post-rollback verification" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario 11 (regression for CHE-530 acceptance item 1): the tuple a
# successful deploy records must itself be admissible as the NEXT deploy's
# baseline.
#
# This is what makes the automatic chain repeatable rather than one-shot.
# deploy.sh used to build this JSON inline with placeholder digests that
# tuple-snapshot.mjs rejects outright (61 hex characters where it requires
# 64), so the second automatic deploy's admission stage would have failed on
# a tuple the first deploy wrote itself. Assert the written state file passes
# the same validator admission.mjs runs, and that its digests are the real
# ones rather than placeholders.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir deployed-tuple-is-admissible)"
control_dir="$state_dir/control"
mkdir -p "$control_dir"
touch "$control_dir/inspect-resolves"
output="$(run_deploy "$state_dir" 2>&1)"
status=$?
expect_exit 0 "$status" deployed-tuple-is-admissible

tuple="$state_dir/deployed-tuple.json"
if ! node deploy/cd/tuple-snapshot.mjs verify --snapshot "$tuple" >/dev/null 2>&1; then
  echo "scenario deployed-tuple-is-admissible: the tuple deploy.sh wrote is not a valid baseline snapshot" >&2
  node deploy/cd/tuple-snapshot.mjs verify --snapshot "$tuple" >&2 || true
  exit 1
fi

# admission.mjs binds a manifest to the SHA-256 of the tuple file, so the
# digest subcommand must also succeed on it.
if ! node deploy/cd/tuple-snapshot.mjs digest --snapshot "$tuple" >/dev/null 2>&1; then
  echo "scenario deployed-tuple-is-admissible: could not digest the written tuple for manifest binding" >&2
  exit 1
fi

tuple_json="$(cat "$tuple")"
expect_not_contains "$tuple_json" "0000000000000000000000000000000000000000000000000000000000000" deployed-tuple-is-admissible
expect_contains "$tuple_json" "$backend_digest" deployed-tuple-is-admissible
expect_contains "$tuple_json" "\"application_sha\": \"$source_sha\"" deployed-tuple-is-admissible


# ---------------------------------------------------------------------------
# Scenario 12 (CHE-549): GHCR_PULL_TOKEN set -> deploy.sh logs in to
# ghcr.io with the real token before pulling, and logs out on the normal
# success exit path.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir ghcr-login-happy-path)"
control_dir="$state_dir/control"
mkdir -p "$control_dir"
touch "$control_dir/inspect-resolves"
export GHCR_PULL_TOKEN=super-secret-pat
output="$(run_deploy "$state_dir" 2>&1)"
status=$?
unset GHCR_PULL_TOKEN
expect_exit 0 "$status" ghcr-login-happy-path
expect_contains "$output" "logging in to ghcr.io" ghcr-login-happy-path

auth_log="$(cat "$control_dir/registry-auth.log")"
expect_contains "$auth_log" "login ghcr.io password=super-secret-pat" ghcr-login-happy-path
expect_contains "$auth_log" "logout ghcr.io" ghcr-login-happy-path
expect_not_contains "$output" "super-secret-pat" ghcr-login-happy-path

# ---------------------------------------------------------------------------
# Scenario 13 (CHE-549): GHCR login itself fails -> treated as a deploy
# failure (rollback path), and the EXIT trap still logs out even though the
# failure happened before the pull step ever ran.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir ghcr-login-fails)"
control_dir="$state_dir/control"
mkdir -p "$control_dir"
touch "$control_dir/inspect-resolves"
touch "$control_dir/fail-ghcr-login"
export GHCR_PULL_TOKEN=super-secret-pat
set +e
output="$(run_deploy "$state_dir" 2>&1)"
status=$?
set -e
unset GHCR_PULL_TOKEN
expect_exit 1 "$status" ghcr-login-fails
expect_contains "$output" "ghcr.io login failed" ghcr-login-fails
expect_contains "$output" "no previous tuple to roll back to" ghcr-login-fails

auth_log="$(cat "$control_dir/registry-auth.log")"
expect_not_contains "$auth_log" "login ghcr.io password=" ghcr-login-fails
expect_contains "$auth_log" "logout ghcr.io" ghcr-login-fails

# ---------------------------------------------------------------------------
# Deployment tuple state and A/B cutover state are separate durable roots.
# The automatic release-candidate path must use the explicit cutover root for
# active_colour instead of assuming C00_STATE_DIR contains both contracts.
workflow_text="$(cat "$root_dir/.github/workflows/cd-deploy.yml")"
expect_contains "$workflow_text" 'C00_CUTOVER_STATE_DIR: ${{ secrets.C00_CUTOVER_STATE_DIR }}' workflow-active-colour-path
expect_contains "$workflow_text" '${C00_CUTOVER_STATE_DIR%/}/cutover-state.json' workflow-active-colour-path
expect_not_contains "$workflow_text" '${C00_STATE_DIR%/}/cutover-state.json' workflow-active-colour-path
expect_not_contains "$workflow_text" '/deploy/cd/router/state/active.json' workflow-active-colour-path

# D2 must invoke the installed A/B controller, not the legacy single-slot
# controller. Base backend/frontend services stay stopped because cutover.sh
# refuses while either is running and only starts colour-suffixed services.
expect_contains "$workflow_text" 'deploy/cd/cutover.sh' workflow-ab-deploy-route
expect_contains "$workflow_text" 'C00_CUTOVER_STATE_DIR: ${{ secrets.C00_CUTOVER_STATE_DIR }}' workflow-ab-deploy-route
expect_contains "$workflow_text" "cutover-controller/evidence/release-packet.json" workflow-ab-deploy-route
expect_contains "$workflow_text" 'capture-release-state.sh' workflow-release-packet
expect_contains "$workflow_text" 'cp "$MANIFEST_PATH" cutover-input/manifest.json' workflow-release-packet
expect_contains "$workflow_text" 'cp "$BASELINE_TUPLE_PATH" cutover-input/baseline-tuple.json' workflow-release-packet
expect_contains "$workflow_text" 'path: cutover-input' workflow-release-packet
expect_contains "$workflow_text" 'build-cutover-bundle.sh' workflow-release-packet
expect_contains "$workflow_text" 'verify-cutover-bundle.sh' workflow-release-packet
expect_contains "$workflow_text" 'sha256sum -c cutover-controller.tar.sha256' workflow-release-packet
# CHE-702: the router state dir comes from the running router's mount,
# never from the Compose checkout path, and feeds both the controller and
# the post-deploy health check.
expect_not_contains "$workflow_text" 'router_state_dir="${C00_COMPOSE_DIR%/}/deploy/cd/router/state"' workflow-router-state
expect_contains "$workflow_text" "router_state_dir=\"\$(\"\${ssh_cmd[@]}\" \"bash '\$resolve_dir/resolve-router-state-dir.sh'\")\"" workflow-router-state
expect_contains "$workflow_text" 'refusing before drain' workflow-router-state
expect_contains "$workflow_text" '--router-state-dir "$router_state_dir"' workflow-router-state
expect_contains "$workflow_text" "--router-state-dir '\$router_state_dir'" workflow-router-state
expect_contains "$workflow_text" 'render-cutover-remote-script.sh' workflow-router-state
remote_script="$(GHCR_PULL_TOKEN=test-token bash deploy/cd/render-cutover-remote-script.sh --router-state-dir '/durable router/state' -- bash -c 'printf %s "$ROUTER_STATE_DIR"')"
expect_contains "$remote_script" 'export ROUTER_STATE_DIR=' workflow-router-state
output="$(env -i PATH="$PATH" bash -s <<<"$remote_script")"
expect_contains "$output" '/durable router/state' workflow-router-state
expect_not_contains "$workflow_text" 'local_packet="cd-deploy-manifest/release-packet.json"' workflow-release-packet
expect_contains "$workflow_text" 'CUTOVER_DATABASE_URL=' workflow-ab-deploy-route
expect_not_contains "$workflow_text" "bash '%s/deploy.sh' --manifest" workflow-ab-deploy-route
expect_not_contains "$workflow_text" 'deploy/cd/deploy.sh deploy/cd/deploy-lib.sh' workflow-ab-deploy-route
expect_not_contains "$workflow_text" 'compose up -d --no-deps backend frontend' workflow-ab-deploy-route

# ---------------------------------------------------------------------------
# The `-- bash -c "..."` cutover command is one big double-quoted string
# nested inside the workflow's own `run: |` block; `expect_contains` above
# only proves substrings are present, not that the nested string is still
# balanced quote-for-quote. A 2026-09-24 edit dropped `-multica}` out of
# `${POSTGRES_USER:-multica}`, leaving `${POSTGRES_USER:***@postgres` — one
# unclosed `${` that swallowed the rest of the line, including the intended
# closing `\"`, and produced `bash: -c: line 1: unexpected EOF while looking
# for matching '"'` only once C00's SSH session tried to parse it (CD run
# 35978855184). Nothing that greps the YAML text catches this: the outer
# `-c "<whole string>"` argument is itself well-formed either way. Reproduce
# the actual failure by rendering the real line through
# render-cutover-remote-script.sh (the same renderer the workflow calls) and
# executing it, exactly like the SSH session does.
remote_cmd_line="$(grep -F -f <(printf '%s' '-- bash -c "cd '"'"'$remote_dir'"'"'') "$root_dir/.github/workflows/cd-deploy.yml")"
remote_cmd_inner="${remote_cmd_line#*-- bash -c \"}"
remote_cmd_inner="${remote_cmd_inner%\\}"
remote_cmd_inner="${remote_cmd_inner% }"
remote_cmd_inner="${remote_cmd_inner%\"}"
[ -n "$remote_cmd_inner" ] || { echo "scenario workflow-cutover-quoting: could not extract the -- bash -c command from cd-deploy.yml" >&2; exit 1; }

cutover_quoting_dir="$(fresh_scenario_dir cutover-quoting)"
remote_dir="$cutover_quoting_dir/remote"
C00_COMPOSE_DIR="$cutover_quoting_dir/compose"
C00_CUTOVER_STATE_DIR="$cutover_quoting_dir/state"
router_state_dir="$cutover_quoting_dir/router"
mkdir -p "$remote_dir" "$C00_COMPOSE_DIR" "$router_state_dir"

# Expand the workflow's own shell variables the same way bash's double-quote
# interpolation does when the job runs — this is what actually reaches
# render-cutover-remote-script.sh as its command argument, not a second
# re-parse of literal text.
expanded_remote_cmd="$(eval "printf '%s' \"$remote_cmd_inner\"")"
rendered_cutover_script="$(GHCR_PULL_TOKEN=test-token bash deploy/cd/render-cutover-remote-script.sh --router-state-dir "$router_state_dir" -- bash -c "$expanded_remote_cmd")"
printf '%s' "$rendered_cutover_script" >"$cutover_quoting_dir/rendered.sh"

set +e
cutover_quoting_output="$(bash "$cutover_quoting_dir/rendered.sh" 2>&1)"
cutover_quoting_status=$?
set -e

# A quoting break fails immediately with bash's own parse error, before any
# real command in the string ever runs. A sound command instead reaches and
# fails on the first real, expected precondition (the sha256 fixture this
# scenario deliberately never creates) — proving the nested `-c "..."`
# argument parsed as one well-formed command.
expect_not_contains "$cutover_quoting_output" "unexpected EOF" workflow-cutover-quoting
expect_not_contains "$cutover_quoting_output" "unexpected end of file" workflow-cutover-quoting
expect_exit 1 "$cutover_quoting_status" workflow-cutover-quoting
expect_contains "$cutover_quoting_output" "cutover-controller.tar.sha256" workflow-cutover-quoting

echo "deploy.sh control-flow fixtures passed"
