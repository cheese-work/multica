#!/usr/bin/env bash
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
GUARD_SCRIPT="$SCRIPT_DIR/go-test-with-agent-cli-guard.sh"

usage() {
  echo "usage: $0 [--race] [--only regular|agent]" >&2
}

# The suite is two `go test` invocations: every package outside pkg/agent at
# the default parallelism, then pkg/agent throttled (see below). `--only`
# selects one half so CI can give each its own runner; the default still runs
# both for `make test`, check.sh, and the release workflow.
go_test_args=(test)
only=all
while [ "$#" -gt 0 ]; do
  case "$1" in
    --race)
      go_test_args+=(-race)
      shift
      ;;
    --only)
      case "${2:-}" in
        regular|agent) only=$2 ;;
        *)
          usage
          exit 2
          ;;
      esac
      shift 2
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

cd "$REPO_ROOT/server"

# CHE-4270: merge_group sets MULTICA_TEST_RERUN_FAILS=1. A flaky test must not
# eject the PR from the queue, so failed tests rerun once, by name, in three
# steps: (1) first pass of every run, no retry; (2) one job-wide decision from
# the failures of ALL runs, the handler shards included: rerun only when 1 to 3
# tests failed and none of them is a panic, a data race, a build or TestMain
# failure (a plan line `!`); (3) the rerun. A test that fails then passes is a
# flake: the job passes and the test shows in $MULTICA_FLAKE_DIR/flakes.json
# (go-test-flakes.mjs). Any other run keeps plain `go test`.
rerun_on() { [ "${MULTICA_TEST_RERUN_FAILS:-}" = 1 ]; }
flake_dir=${MULTICA_FLAKE_DIR:-$REPO_ROOT/flake-reports}
FLAKES_SCRIPT="$SCRIPT_DIR/go-test-flakes.mjs"

# First pass. $1: name, $2: space-separated packages, rest: go test flags.
run_go_test() {
  local name=$1 packages=$2
  shift 2
  if ! rerun_on; then
    "$GUARD_SCRIPT" -- go test "$@" $packages
    return
  fi
  mkdir -p "$flake_dir"
  "$GUARD_SCRIPT" -- gotestsum --format standard-verbose --jsonfile "$flake_dir/go-test-$name.json" \
    --packages "$packages" -- "$@"
}

# Failures of the first pass of run $1 (exit status $2) as plan lines
# `<package>\t<root test>`, or `!` when a rerun proves nothing.
write_plan() {
  node "$FLAKES_SCRIPT" --plan "$flake_dir/go-test-$1.json" >"$flake_dir/plan-$1.txt"
  if [ "$2" -ne 0 ] && [ ! -s "$flake_dir/plan-$1.txt" ]; then echo '!' >"$flake_dir/plan-$1.txt"; fi
}

# Job-wide budget over the given plan files: no `!` line, 1 to 3 failed tests.
rerun_allowed() {
  local lines
  lines=$(cat "$@" 2>/dev/null | sort -u)
  ! printf '%s\n' "$lines" | grep -q '^!' || return 1
  local n
  n=$(printf '%s\n' "$lines" | grep -c .) || true
  [ "$n" -ge 1 ] && [ "$n" -le 3 ]
}

# Rerun the failed tests of plan $2 once, package by package. $1: name, rest: go test flags.
rerun_failed() {
  local name=$1 plan=$2 status=0 pkg tests n=0
  shift 2
  for pkg in $(cut -f1 "$plan" | sort -u); do
    n=$((n + 1))
    tests=$(awk -F'\t' -v p="$pkg" '$1 == p { print $2 }' "$plan" | paste -sd'|')
    echo "rerunning failed tests once in $pkg: $tests"
    "$GUARD_SCRIPT" -- gotestsum --format standard-verbose --jsonfile "$flake_dir/go-test-$name-rerun-$n.json" \
      --packages "$pkg" -- "$@" -count=1 -run "^($tests)\$" || status=$?
  done
  return "$status"
}

if [ "$only" != agent ]; then
  packages=$(go list ./...)
  regular_packages=()
  for package in $packages; do
    case "$package" in
      */pkg/agent|*/pkg/agent/*) ;;
      *) regular_packages+=("$package") ;;
    esac
  done
  # CHE-731: these packages share one Postgres instance and its tables (see
  # testutil/db.go), so the default per-package parallelism raced multiple
  # packages' fixtures against each other and produced cross-package lock
  # waits and timeouts. -p 1 runs packages one at a time; tests within a
  # package keep the default -parallel.
  # CHE-2418: MULTICA_TEST_DB_PER_PACKAGE=1 (CI) removes the sharing instead:
  # go-test-db-exec.sh clones the migrated DATABASE_URL database per package,
  # so -p 4 is safe. Unset, the serial behavior above stands.
  package_parallelism=(-p 1)
  handler_shards_pid=
  if [ "${MULTICA_TEST_DB_PER_PACKAGE:-}" = 1 ]; then
    package_parallelism=(-p 4 -exec "$SCRIPT_DIR/go-test-db-exec.sh")
    # internal/handler alone outlasted every other package combined, so it
    # runs as parallel shards beside the rest (go-test-handler-shards.sh).
    other_packages=()
    for package in "${regular_packages[@]}"; do
      case "$package" in
        */internal/handler) ;;
        *) other_packages+=("$package") ;;
      esac
    done
    regular_packages=("${other_packages[@]}")
    if rerun_on; then
      mkdir -p "$flake_dir"
      rm -f "$flake_dir/handler-failed.txt" "$flake_dir/handler-first-pass.done" "$flake_dir/rerun-decision"
    fi
    "${MULTICA_HANDLER_SHARDS_SCRIPT:-$SCRIPT_DIR/go-test-handler-shards.sh}" "${go_test_args[@]:1}" &
    handler_shards_pid=$!
  fi
  regular_status=0
  run_go_test regular "${regular_packages[*]}" "${go_test_args[@]:1}" "${package_parallelism[@]}" || regular_status=$?
  if rerun_on; then
    write_plan regular "$regular_status"
    plans=("$flake_dir/plan-regular.txt")
    if [ -n "$handler_shards_pid" ]; then
      # The shards publish their first-pass failures, then wait for this decision.
      while [ ! -e "$flake_dir/handler-first-pass.done" ] && kill -0 "$handler_shards_pid" 2>/dev/null; do sleep 1; done
      [ -e "$flake_dir/handler-first-pass.done" ] || echo '!' >"$flake_dir/handler-failed.txt"
      plans+=("$flake_dir/handler-failed.txt")
    fi
    if rerun_allowed "${plans[@]}"; then
      echo rerun >"$flake_dir/rerun-decision.tmp"
    else
      echo skip >"$flake_dir/rerun-decision.tmp"
    fi
    mv "$flake_dir/rerun-decision.tmp" "$flake_dir/rerun-decision"
    if [ "$regular_status" -ne 0 ] && [ "$(cat "$flake_dir/rerun-decision")" = rerun ]; then
      regular_status=0
      rerun_failed regular "$flake_dir/plan-regular.txt" "${go_test_args[@]:1}" "${package_parallelism[@]}" || regular_status=$?
    fi
  fi
  shards_status=0
  [ -z "$handler_shards_pid" ] || wait "$handler_shards_pid" || shards_status=$?
  [ "$regular_status" -eq 0 ] || exit "$regular_status"
  [ "$shards_status" -eq 0 ] || exit "$shards_status"
fi

if [ "$only" != regular ]; then
  # Subprocess-backed agent tests have hard deadlines. Limit both package and
  # within-package parallelism so race builds do not starve their parent loops.
  agent_status=0
  run_go_test agent ./pkg/agent/... "${go_test_args[@]:1}" -p 2 -parallel 2 || agent_status=$?
  if rerun_on && [ "$agent_status" -ne 0 ]; then
    write_plan agent "$agent_status"
    if rerun_allowed "$flake_dir/plan-agent.txt"; then
      agent_status=0
      rerun_failed agent "$flake_dir/plan-agent.txt" "${go_test_args[@]:1}" -p 2 -parallel 2 || agent_status=$?
    fi
  fi
  exit "$agent_status"
fi
