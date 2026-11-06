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

# CHE-4270: merge_group sets MULTICA_TEST_RERUN_FAILS=1. Each half then runs
# under gotestsum, which reruns failed tests once by name (more than 3 failures,
# a panic or a data race: no rerun). A test that fails then passes is a flake:
# the run passes and the test shows in $MULTICA_FLAKE_DIR/go-test-<name>.json
# for go-test-flakes.mjs. Any other run keeps plain `go test`.
# $1: json name, $2: space-separated packages, rest: go test flags.
run_go_test() {
  local name=$1 packages=$2
  shift 2
  if [ "${MULTICA_TEST_RERUN_FAILS:-}" != 1 ]; then
    "$GUARD_SCRIPT" -- go test "$@" $packages
    return
  fi
  local flake_dir=${MULTICA_FLAKE_DIR:-$REPO_ROOT/flake-reports}
  mkdir -p "$flake_dir"
  "$GUARD_SCRIPT" -- gotestsum --rerun-fails=1 --rerun-fails-max-failures=3 --rerun-fails-abort-on-data-race \
    --format standard-verbose --jsonfile "$flake_dir/go-test-$name.json" --packages "$packages" -- "$@"
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
    "${MULTICA_HANDLER_SHARDS_SCRIPT:-$SCRIPT_DIR/go-test-handler-shards.sh}" "${go_test_args[@]:1}" &
    handler_shards_pid=$!
  fi
  regular_status=0
  run_go_test regular "${regular_packages[*]}" "${go_test_args[@]:1}" "${package_parallelism[@]}" || regular_status=$?
  shards_status=0
  [ -z "$handler_shards_pid" ] || wait "$handler_shards_pid" || shards_status=$?
  [ "$regular_status" -eq 0 ] || exit "$regular_status"
  [ "$shards_status" -eq 0 ] || exit "$shards_status"
fi

if [ "$only" != regular ]; then
  # Subprocess-backed agent tests have hard deadlines. Limit both package and
  # within-package parallelism so race builds do not starve their parent loops.
  run_go_test agent ./pkg/agent/... "${go_test_args[@]:1}" -p 2 -parallel 2
fi
