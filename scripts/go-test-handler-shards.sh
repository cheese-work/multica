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

# CHE-4270: in merge_group (MULTICA_TEST_RERUN_FAILS=1) failed tests rerun once
# so a flake does not eject the PR from the queue, but only on a job-wide
# decision: this script publishes its first-pass failures (plan lines
# `internal/handler\t<root>\t<leaf>`, one per failed leaf test, or `!` for a panic, a timeout, a data race or a
# failure with no test name, which a rerun proves nothing about) in
# $MULTICA_FLAKE_DIR/handler-failed.txt, touches handler-first-pass.done, and
# waits for test-go.sh to write rerun-decision (`rerun` or `skip`) after it
# counted the failures of every run. A passing rerun records the tests in
# handler-flakes.txt.
flake_dir=${MULTICA_FLAKE_DIR:-$REPO_ROOT/flake-reports}
rerun_wait=${MULTICA_RERUN_WAIT:-3600}

run_shard() { # $1: -test.run regex, rest: extra test flags
  (cd "$package_dir" && exec "$GUARD_SCRIPT" -- "$EXEC_WRAPPER" "$work_dir/handler.test" \
    -test.run "$1" -test.count=1 -test.timeout="$shard_timeout" ${verbose[@]+"${verbose[@]}"} "${@:2}")
}

first_pass_shard() { # $1: shard index, $2: -test.run regex; writes shard-N.plan, returns the shard status
  local log="$work_dir/shard-$1.log" shard_status=0 failed
  set -o pipefail
  run_shard "$2" 2>&1 | tee "$log" || shard_status=$?
  : >"$work_dir/shard-$1.plan"
  [ "$shard_status" -ne 0 ] || return 0
  # Leaf failures only (a failing subtest fails its parents): `--- FAIL: Root/sub` lines are indented.
  failed=$(grep -oE '^ *--- FAIL: [^ ]+' "$log" | sed -E 's/^ *--- FAIL: //' | sort -u | awk '{ t[NR] = $0 } END { for (i = 1; i <= NR; i++) { leaf = 1; for (j = 1; j <= NR; j++) if (index(t[j], t[i] "/") == 1) leaf = 0; if (leaf) print t[i] } }') || true
  if [ "$shard_status" -ne 1 ] || [ -z "$failed" ] || grep -qE '^panic: |^WARNING: DATA RACE|race detected during execution' "$log"; then
    echo '!' >"$work_dir/shard-$1.plan"
  else
    printf 'internal/handler\t%s\t%s\n' $(printf '%s\n' $failed | awk '{ split($0, a, "/"); print a[1], $0 }') >"$work_dir/shard-$1.plan"
  fi
  return "$shard_status"
}

rerun_on() { [ "${MULTICA_TEST_RERUN_FAILS:-}" = 1 ]; }

pids=()
indexes=()
for ((i = 0; i < shards; i++)); do
  [ -z "$only_shard" ] || [ "$i" -eq "$only_shard" ] || continue
  pattern=$(printf '%s\n' "$names" | awk -v n="$shards" -v i="$i" 'NR % n == i { printf "%s%s", sep, $0; sep = "|" }')
  [ -n "$pattern" ] || continue
  if rerun_on; then
    first_pass_shard "$i" "^($pattern)\$" &
  else
    run_shard "^($pattern)\$" &
  fi
  pids+=($!)
  indexes+=("$i")
done

status=0
for pid in "${pids[@]}"; do
  wait "$pid" || status=1
done
rerun_on || exit "$status"

mkdir -p "$flake_dir"
plans=()
for i in "${indexes[@]}"; do plans+=("$work_dir/shard-$i.plan"); done
cat "${plans[@]}" | sort -u >"$flake_dir/handler-failed.txt"
touch "$flake_dir/handler-first-pass.done"
[ "$status" -ne 0 ] || exit 0

waited=0
while [ ! -e "$flake_dir/rerun-decision" ]; do
  if [ "$waited" -ge "$rerun_wait" ]; then
    echo "no rerun decision after ${rerun_wait}s: failing" >&2
    exit 1
  fi
  sleep 1
  waited=$((waited + 1))
done
[ "$(cat "$flake_dir/rerun-decision")" = rerun ] || exit 1

roots=$(cut -f2 "$flake_dir/handler-failed.txt" | sort -u)
echo "rerunning failed tests once: $(printf '%s' "$roots" | paste -sd' ')"
# A root rerun can skip a failed subtest, so a leaf only counts as recovered with its own PASS line.
rerun_log="$work_dir/rerun.log"
set -o pipefail
run_shard "^($(printf '%s' "$roots" | paste -sd'|'))\$" -test.v 2>&1 | tee "$rerun_log" || exit 1
unproven=0
while IFS=$'\t' read -r _ _ leaf; do
  grep -qF -- "--- PASS: $leaf (" "$rerun_log" || { echo "rerun did not prove $leaf passed" >&2; unproven=1; }
done <"$flake_dir/handler-failed.txt"
[ "$unproven" -eq 0 ] || exit 1
cut -f1,3 "$flake_dir/handler-failed.txt" >>"$flake_dir/handler-flakes.txt"
