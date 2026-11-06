#!/usr/bin/env bash
# CHE-2418 B1: internal/handler is one package, so `go test -p 4` ran it in a
# single process (392 s under -race, the floor of the backend Test step). Build
# its test binary once, then run it as MULTICA_HANDLER_SHARDS processes (default
# 3), each taking every Nth top-level test and, via go-test-db-exec.sh, its own
# cloned database. Same runner, same cores: the package just stops being serial.
# CHE-4268: MULTICA_HANDLER_SHARD_INDEX=N (0-based) runs only shard N, so CI can
# give each shard its own runner; every job still computes the same split.
# Usage: go-test-handler-shards.sh [go test build flags, e.g. -race]
# ponytail: tests are dealt round-robin by name, not by measured cost; the
# longest test is ~18 s, so shards stay close. Weight them if that stops holding.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
GUARD_SCRIPT="$SCRIPT_DIR/go-test-with-agent-cli-guard.sh"
EXEC_WRAPPER=${MULTICA_TEST_EXEC:-$SCRIPT_DIR/go-test-db-exec.sh}
shards=${MULTICA_HANDLER_SHARDS:-3}
package_dir="$REPO_ROOT/server/internal/handler"

case $shards in
  '' | *[!0-9]* | 0*) echo "MULTICA_HANDLER_SHARDS must be an integer >= 1, got '$shards'" >&2; exit 2 ;;
esac

only_shard=${MULTICA_HANDLER_SHARD_INDEX:-}
if [ -n "$only_shard" ] && { [[ ! $only_shard =~ ^(0|[1-9][0-9]*)$ ]] || [ "$only_shard" -ge "$shards" ]; }; then
  echo "MULTICA_HANDLER_SHARD_INDEX must be an integer in 0..$((shards - 1)), got '$only_shard'" >&2
  exit 2
fi

# CHE-4243: the shard binaries are not bound by `go test`'s default timeout, so
# this is their only one. A shard sums to ~5-8 min on a healthy runner (12 runs,
# 6 Nov 2026) and a slow runner stretched one past the old fixed 10m. 20m keeps
# a hang bounded while leaving ~2.5x headroom over the median shard.
shard_timeout=${MULTICA_HANDLER_SHARD_TIMEOUT:-20m}
case $shard_timeout in
  [1-9]*[smh]) case ${shard_timeout%[smh]} in *[!0-9]*) shard_timeout= ;; esac ;;
  *) shard_timeout= ;;
esac
if [ -z "$shard_timeout" ]; then
  echo "MULTICA_HANDLER_SHARD_TIMEOUT must be <integer>s|m|h, got '${MULTICA_HANDLER_SHARD_TIMEOUT}'" >&2
  exit 2
fi

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/multica-handler-shards.XXXXXX")
trap 'rm -rf "$work_dir"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

(cd "$REPO_ROOT/server" && go test -c "$@" -o "$work_dir/handler.test" ./internal/handler)

# Top-level names come from source: `-list` would run TestMain, which needs a
# database. gofmt keeps declarations at column 0, so this finds them all.
names=$(grep -hoE '^func (Test|Example|Fuzz)[A-Za-z0-9_]*\(' "$package_dir"/*_test.go \
  | sed -E 's/^func ([^(]*)\(/\1/' | sort -u)
if [ -z "$names" ]; then
  echo "no tests found in $package_dir" >&2
  exit 1
fi

# `go test -v` reaches a direct binary only as -test.v.
verbose=()
case " ${GOFLAGS:-} " in *" -v "*) verbose=(-test.v) ;; esac

pids=()
for ((i = 0; i < shards; i++)); do
  [ -z "$only_shard" ] || [ "$i" -eq "$only_shard" ] || continue
  pattern=$(printf '%s\n' "$names" | awk -v n="$shards" -v i="$i" 'NR % n == i { printf "%s%s", sep, $0; sep = "|" }')
  [ -n "$pattern" ] || continue
  (cd "$package_dir" && exec "$GUARD_SCRIPT" -- "$EXEC_WRAPPER" "$work_dir/handler.test" \
    -test.run "^($pattern)\$" -test.count=1 -test.timeout="$shard_timeout" ${verbose[@]+"${verbose[@]}"}) &
  pids+=($!)
done

status=0
for pid in "${pids[@]}"; do
  wait "$pid" || status=1
done
exit "$status"
