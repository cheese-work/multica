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
  # docker inspect --format '{{index .RepoDigests 0}}' repo:tag
  ref="${*: -1}"
  repo="${ref%%:*}"
  digest=""
  case "$repo" in
    *multica-backend) digest="${MOCK_BACKEND_DIGEST:-}" ;;
    *multica-web) digest="${MOCK_WEB_DIGEST:-}" ;;
  esac
  if [ -n "$digest" ] && [ -f "$control_dir/inspect-resolves" ]; then
    printf '%s@%s\n' "$repo" "$digest"
  fi
  exit 0
fi

if [ "$1" = "compose" ]; then
  shift
  # shift off "-f docker-compose.selfhost.yml"
  if [ "$1" = "-f" ]; then shift 2; fi
  sub="$1"; shift
  case "$sub" in
    pull)
      fail_if_flagged fail-pull
      exit 0
      ;;
    run)
      # run --rm --no-deps --entrypoint ./migrate backend up|down --to <v>
      args=("$@")
      direction=""
      for a in "${args[@]}"; do
        case "$a" in
          up) direction="up" ;;
          down) direction="down" ;;
        esac
      done
      if [ "$direction" = "down" ]; then
        fail_if_flagged fail-migrate-down
      else
        fail_if_flagged fail-migrate-up
      fi
      exit 0
      ;;
    up)
      # up -d --no-deps backend web — used both for the forward deploy and
      # for rollback's restart of the previous tuple. Distinguish which one
      # via separate flags so a test can fail just one of the two.
      if [ -f "$control_dir/rollback-in-progress" ]; then
        fail_if_flagged fail-rollback-restart
      else
        fail_if_flagged fail-container-start
      fi
      touch "$control_dir/containers-up"
      exit 0
      ;;
    stop)
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
      # Both call sites (capture_tuple's count/version/applied_at query and
      # rollback's version-only query) parse a single line; emit a shape
      # that satisfies either awk -F'|' split or the bare psql -tA output.
      printf '1|%s|2026-09-12T08:58:23.324992Z\n' "$version"
      exit 0
      ;;
    images)
      # images backend --format json — empty is fine: capture_tuple falls
      # back to "${backend_repo}:${image_tag}" when this resolves nothing.
      printf '[]\n'
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
set +e
output="$(run_deploy "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" placeholder-digest-fallback
expect_contains "$output" "falling back to tag reference" placeholder-digest-fallback
expect_not_contains "$output" "@$placeholder_digest" placeholder-digest-fallback

echo "deploy.sh control-flow fixtures passed"
