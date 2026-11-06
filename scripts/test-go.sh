#!/usr/bin/env bash
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
GUARD_SCRIPT="$SCRIPT_DIR/go-test-with-agent-cli-guard.sh"

usage() {
  echo "usage: $0 [--race] [--only regular|agent] [--partition NAME]" >&2
}

# The suite is two `go test` invocations: every package outside pkg/agent at
# the default parallelism, then pkg/agent throttled (see below). `--only`
# selects one half so CI can give each its own runner; the default still runs
# both for `make test`, check.sh, and the release workflow.
# CHE-4268: `--partition` cuts the regular half so CI runs it as parallel jobs:
# handler-N = shard N of internal/handler, service = internal/service, rest =
# every other regular package. The partitions together equal the whole half.
go_test_args=(test)
only=all
partition=
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
    --partition)
      case "${2:-}" in
        handler-[0-9]*|service|rest) partition=$2 ;;
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

# A partition is a slice of the regular half and needs the per-package
# databases the parallel CI run uses; without them slices would race shared tables.
if [ -n "$partition" ]; then
  [ "$only" != agent ] || { usage; exit 2; }
  case "$partition" in
    handler-*[!0-9]*) usage; exit 2 ;;
  esac
  if [ "${MULTICA_TEST_DB_PER_PACKAGE:-}" != 1 ]; then
    echo "--partition needs MULTICA_TEST_DB_PER_PACKAGE=1" >&2
    exit 2
  fi
  only=regular
fi

cd "$REPO_ROOT/server"

# CHE-4270: merge_group sets MULTICA_TEST_RERUN_FAILS=1. A flaky test must not
# eject the PR from the queue, so failed tests rerun once, by name, in three
# steps: (1) first pass of every run, no retry; (2) one job-wide decision from
# the failures of ALL runs, the handler shards included: rerun only when 1 to 3
# tests failed and none of them is a panic, a data race, a build or TestMain
# failure (a plan line `!`); (3) the rerun. A test that fails then passes is a
# flake: the job passes and the test shows in $MULTICA_FLAKE_DIR/flakes.json
# (go-test-flakes.mjs). Any other run keeps plain `go test`.
# CHE-4268: in CI each partition is its own job, so "job-wide" means that partition.
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
# `<package>\t<root test>\t<leaf test>` (one per failed leaf, subtests included), or `!` when a rerun proves nothing.
write_plan() {
  node "$FLAKES_SCRIPT" --plan "$flake_dir/go-test-$1.json" >"$flake_dir/plan-$1.txt"
  if [ "$2" -ne 0 ] && [ ! -s "$flake_dir/plan-$1.txt" ]; then echo '!' >"$flake_dir/plan-$1.txt"; fi
}

# Job-wide budget over the given plan files: no `!` line, 1 to 3 failed leaf tests.
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
  local name=$1 plan=$2 status=0 pkg tests n=0 unproven reruns=()
  shift 2
  for pkg in $(cut -f1 "$plan" | sort -u); do
    n=$((n + 1))
    tests=$(awk -F'\t' -v p="$pkg" '$1 == p { print $2 }' "$plan" | sort -u | paste -sd'|')
    echo "rerunning failed tests once in $pkg: $tests"
    "$GUARD_SCRIPT" -- gotestsum --format standard-verbose --jsonfile "$flake_dir/go-test-$name-rerun-$n.json" \
      --packages "$pkg" -- "$@" -count=1 -run "^($tests)\$" || status=$?
    reruns+=("$flake_dir/go-test-$name-rerun-$n.json")
  done
  # A root rerun can skip a failed subtest; a leaf with no pass event did not recover.
  # errexit is off here (callers use ||), so a parser failure must be handled by hand.
  unproven=$(node "$FLAKES_SCRIPT" --unproven "$plan" "${reruns[@]}") || {
    echo "could not read the rerun results; nothing is proven" >&2
    return 1
  }
  if [ -n "$unproven" ]; then
    echo "rerun did not prove these tests passed: $(printf '%s' "$unproven" | tr '\t\n' ': ')" >&2
    status=1
  fi
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
      case "$package:$partition" in
        */internal/handler:*) ;;
        */internal/service: | */internal/service:service) other_packages+=("$package") ;;
        */internal/service:*) ;;
        *: | *:rest) other_packages+=("$package") ;;
      esac
    done
    regular_packages=("${other_packages[@]}")
    if [ -z "$partition" ] || [[ "$partition" == handler-* ]]; then
      if rerun_on; then
        mkdir -p "$flake_dir"
        rm -f "$flake_dir/handler-failed.txt" "$flake_dir/handler-first-pass.done" "$flake_dir/rerun-decision"
      fi
      MULTICA_HANDLER_SHARD_INDEX=${partition#handler-} \
        "${MULTICA_HANDLER_SHARDS_SCRIPT:-$SCRIPT_DIR/go-test-handler-shards.sh}" "${go_test_args[@]:1}" &
      handler_shards_pid=$!
    fi
  fi
  regular_status=0
  # A handler-N partition has no regular package: its plan is empty and only the shard counts.
  if [ "${#regular_packages[@]}" -gt 0 ]; then
    run_go_test regular "${regular_packages[*]}" "${go_test_args[@]:1}" "${package_parallelism[@]}" || regular_status=$?
  fi
  if rerun_on; then
    mkdir -p "$flake_dir"
    if [ "${#regular_packages[@]}" -gt 0 ]; then write_plan regular "$regular_status"; else : >"$flake_dir/plan-regular.txt"; fi
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
