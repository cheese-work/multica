#!/usr/bin/env bash
# CHE-2418 B1: internal/handler is one package, so `go test -p 4` ran it in a
# single process (392 s under -race, the floor of the backend Test step). Build
# its test binary once, then run it as MULTICA_HANDLER_SHARDS processes (default
# 3), each taking every Nth top-level test and, via go-test-db-exec.sh, its own
# cloned database. Same runner, same cores: the package just stops being serial.
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

# CHE-4270: in merge_group (MULTICA_TEST_RERUN_FAILS=1) a shard that failed on
# 1-3 named top-level tests reruns just those once, so a flake does not eject
# the PR from the queue. Passing on rerun records the test in
# $MULTICA_FLAKE_DIR/handler-flakes.txt. Not rerun: exit != 1 (panic, timeout,
# data race leave later tests unrun), no `--- FAIL` name, or more than 3 names.
flake_dir=${MULTICA_FLAKE_DIR:-$REPO_ROOT/flake-reports}

run_shard() { # $1: -test.run regex
  (cd "$package_dir" && exec "$GUARD_SCRIPT" -- "$EXEC_WRAPPER" "$work_dir/handler.test" \
    -test.run "$1" -test.count=1 -test.timeout=10m ${verbose[@]+"${verbose[@]}"})
}

run_shard_with_rerun() { # $1: shard index, $2: -test.run regex
  local log="$work_dir/shard-$1.log" shard_status=0 failed count
  set -o pipefail
  run_shard "$2" 2>&1 | tee "$log" || shard_status=$?
  [ "$shard_status" -eq 1 ] || return "$shard_status"
  grep -q '^panic: ' "$log" && return 1
  failed=$(grep -oE '^--- FAIL: [A-Za-z0-9_]+' "$log" | sed 's/^--- FAIL: //' | sort -u) || true
  count=$(printf '%s' "$failed" | grep -c .) || true
  { [ "$count" -ge 1 ] && [ "$count" -le 3 ]; } || return 1
  echo "rerunning failed tests once: $(printf '%s' "$failed" | paste -sd' ')"
  run_shard "^($(printf '%s' "$failed" | paste -sd'|'))\$" || return $?
  mkdir -p "$flake_dir"
  printf 'internal/handler\t%s\n' $failed >>"$flake_dir/handler-flakes.txt"
}

pids=()
for ((i = 0; i < shards; i++)); do
  pattern=$(printf '%s\n' "$names" | awk -v n="$shards" -v i="$i" 'NR % n == i { printf "%s%s", sep, $0; sep = "|" }')
  [ -n "$pattern" ] || continue
  if [ "${MULTICA_TEST_RERUN_FAILS:-}" = 1 ]; then
    run_shard_with_rerun "$i" "^($pattern)\$" &
  else
    run_shard "^($pattern)\$" &
  fi
  pids+=($!)
done

status=0
for pid in "${pids[@]}"; do
  wait "$pid" || status=1
done
exit "$status"
