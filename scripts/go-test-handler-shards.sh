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
  pattern=$(printf '%s\n' "$names" | awk -v n="$shards" -v i="$i" 'NR % n == i { printf "%s%s", sep, $0; sep = "|" }')
  [ -n "$pattern" ] || continue
  (cd "$package_dir" && exec "$GUARD_SCRIPT" -- "$EXEC_WRAPPER" "$work_dir/handler.test" \
    -test.run "^($pattern)\$" -test.count=1 -test.timeout=10m ${verbose[@]+"${verbose[@]}"}) &
  pids+=($!)
done

status=0
for pid in "${pids[@]}"; do
  wait "$pid" || status=1
done
exit "$status"
