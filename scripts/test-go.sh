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
      MULTICA_HANDLER_SHARD_INDEX=${partition#handler-} \
        "${MULTICA_HANDLER_SHARDS_SCRIPT:-$SCRIPT_DIR/go-test-handler-shards.sh}" "${go_test_args[@]:1}" &
      handler_shards_pid=$!
    fi
  fi
  regular_status=0
  [ "${#regular_packages[@]}" -eq 0 ] ||
    "$GUARD_SCRIPT" -- go "${go_test_args[@]}" "${package_parallelism[@]}" "${regular_packages[@]}" || regular_status=$?
  shards_status=0
  [ -z "$handler_shards_pid" ] || wait "$handler_shards_pid" || shards_status=$?
  [ "$regular_status" -eq 0 ] || exit "$regular_status"
  [ "$shards_status" -eq 0 ] || exit "$shards_status"
fi

if [ "$only" != regular ]; then
  # Subprocess-backed agent tests have hard deadlines. Limit both package and
  # within-package parallelism so race builds do not starve their parent loops.
  "$GUARD_SCRIPT" -- go "${go_test_args[@]}" -p 2 -parallel 2 ./pkg/agent/...
fi
