#!/usr/bin/env bash
set -euo pipefail

# Behavioral test for router.sh (CHE-397 unit 2): render -> validate ->
# atomic activate -> reload -> revert, plus the negative controls the issue
# requires (invalid router configuration must genuinely fail).
#
# Uses the real nginx:1.27-alpine image for `nginx -t` (via Docker) so
# validation behavior matches production exactly, but never starts a real
# long-running router container — ROUTER_RELOAD_CMD substitutes a no-op or
# a scripted failure for the reload step, which is the only part of
# router.sh that would otherwise need one.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

if ! command -v docker >/dev/null 2>&1; then
  echo "docker not available; skipping test-router.sh (nginx -t validation requires it)" >&2
  exit 0
fi

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

expect_exit() {
  local want=$1 got=$2 name=$3
  if [ "$got" -ne "$want" ]; then
    echo "scenario $name: exit $got, want $want" >&2
    exit 1
  fi
}

expect_contains() {
  local haystack=$1 needle=$2 name=$3
  if [[ "$haystack" != *"$needle"* ]]; then
    echo "scenario $name: output missing expected text: $needle" >&2
    echo "--- captured output ---" >&2
    echo "$haystack" >&2
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# Scenario 1: select blue -> active.conf/.json point at blue's ports, reload
# invoked, generation retained in history.
# ---------------------------------------------------------------------------
state_dir="$work_dir/scenario1"
export ROUTER_RELOAD_CMD='true'
output="$(bash deploy/cd/router.sh select --colour blue --state-dir "$state_dir" 2>&1)"
status=$?
expect_exit 0 "$status" select-blue
expect_contains "$output" "router generation activated: colour=blue" select-blue
if [ ! -L "$state_dir/active.conf" ]; then
  echo "scenario select-blue: active.conf is not a symlink" >&2
  exit 1
fi
active_colour="$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).colour)' "$state_dir/active.json")"
if [ "$active_colour" != "blue" ]; then
  echo "scenario select-blue: active.json colour is '$active_colour', want blue" >&2
  exit 1
fi
if ! grep -q "127.0.0.1:${BACKEND_BLUE_PORT:-18081}" "$(readlink -f "$state_dir/active.conf")"; then
  echo "scenario select-blue: active generation does not reference blue's backend port" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario 2: select green after blue -> revert restores blue. This is the
# "retain the old generation for reversion" requirement from the accepted
# architecture, exercised end to end.
# ---------------------------------------------------------------------------
bash deploy/cd/router.sh select --colour green --state-dir "$state_dir" >/dev/null 2>&1
active_colour="$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).colour)' "$state_dir/active.json")"
expect_exit 0 0 select-green # select itself already asserted exit above via set -e
if [ "$active_colour" != "green" ]; then
  echo "scenario select-green: active.json colour is '$active_colour', want green" >&2
  exit 1
fi

output="$(bash deploy/cd/router.sh revert --state-dir "$state_dir" 2>&1)"
status=$?
expect_exit 0 "$status" revert-to-blue
active_colour="$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).colour)' "$state_dir/active.json")"
if [ "$active_colour" != "blue" ]; then
  echo "scenario revert-to-blue: active.json colour is '$active_colour' after revert, want blue" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario 3 (negative control — must genuinely fail): a corrupted template
# fails nginx -t and select must refuse to activate it. The active
# generation (blue, from the revert above) must remain unchanged.
# ---------------------------------------------------------------------------
state_dir3="$work_dir/scenario3"
bash deploy/cd/router.sh select --colour blue --state-dir "$state_dir3" >/dev/null 2>&1
before_target="$(readlink -f "$state_dir3/active.conf")"

# router.sh derives root_dir from its OWN path (dirname/../..), so the
# corrupted copy must live at the same relative depth (<root>/deploy/cd/) as
# the real one for that resolution to find the corrupted template instead of
# the real one.
broken_root="$work_dir/broken-root"
mkdir -p "$broken_root/deploy/cd/router"
cp deploy/cd/router/nginx.conf.template "$broken_root/deploy/cd/router/nginx.conf.template"
printf '\ngarbage {{{ not valid nginx\n' >>"$broken_root/deploy/cd/router/nginx.conf.template"
cp deploy/cd/router.sh "$broken_root/deploy/cd/router.sh"

set +e
output="$(bash "$broken_root/deploy/cd/router.sh" select --colour green --state-dir "$state_dir3" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" invalid-config-rejected
expect_contains "$output" "failed nginx -t" invalid-config-rejected
after_target="$(readlink -f "$state_dir3/active.conf")"
if [ "$before_target" != "$after_target" ]; then
  echo "scenario invalid-config-rejected: active.conf changed despite validation failure (before=$before_target after=$after_target)" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario 4 (negative control — must genuinely fail): reload failure is
# reported as an error even though the config was already swapped — the
# caller (cutover.sh) must be able to detect "config selected but router
# process never picked it up".
# ---------------------------------------------------------------------------
state_dir4="$work_dir/scenario4"
export ROUTER_RELOAD_CMD='exit 1'
set +e
output="$(bash deploy/cd/router.sh select --colour blue --state-dir "$state_dir4" 2>&1)"
status=$?
set -e
unset ROUTER_RELOAD_CMD
expect_exit 1 "$status" reload-failure-reported
expect_contains "$output" "router reload failed" reload-failure-reported

# ---------------------------------------------------------------------------
# Scenario 5 (negative control — must genuinely fail): revert with no prior
# generation recorded.
# ---------------------------------------------------------------------------
state_dir5="$work_dir/scenario5"
mkdir -p "$state_dir5"
set +e
output="$(bash deploy/cd/router.sh revert --state-dir "$state_dir5" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" revert-with-no-history
expect_contains "$output" "cannot revert" revert-with-no-history

# ---------------------------------------------------------------------------
# Scenario 6: probe reports the active colour and fails when that colour's
# backend is not actually reachable (no real backend running in this test).
# ---------------------------------------------------------------------------
state_dir6="$work_dir/scenario6"
export ROUTER_RELOAD_CMD='true'
bash deploy/cd/router.sh select --colour green --state-dir "$state_dir6" >/dev/null 2>&1
set +e
output="$(bash deploy/cd/router.sh probe --state-dir "$state_dir6" 2>&1)"
status=$?
set -e
unset ROUTER_RELOAD_CMD
expect_exit 1 "$status" probe-backend-unreachable
expect_contains "$output" "green" probe-backend-unreachable

echo "router.sh control-flow fixtures passed"
