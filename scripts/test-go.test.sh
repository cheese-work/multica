#!/usr/bin/env bash
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
TEST_DIR=$(mktemp -d "${TMPDIR:-/tmp}/multica-test-go.XXXXXX")
BIN_DIR="$TEST_DIR/bin"
CALLS_FILE="$TEST_DIR/go-calls.log"
OUTPUT_FILE="$TEST_DIR/output.log"

cleanup() {
  rm -rf "$TEST_DIR"
}
trap cleanup EXIT

mkdir -p "$BIN_DIR"
export MULTICA_TEST_GO_CALLS="$CALLS_FILE"
: >"$CALLS_FILE"

cat >"$BIN_DIR/go" <<'FAKE'
#!/usr/bin/env bash
set -eu

case "${1:-}" in
  list)
    if [ "$#" -ne 2 ] || [ "$2" != "./..." ]; then
      echo "unexpected go list arguments: $*" >&2
      exit 2
    fi
    printf '%s\n' \
      github.com/multica-ai/multica/server \
      github.com/multica-ai/multica/server/internal/daemon \
      github.com/multica-ai/multica/server/internal/handler \
      github.com/multica-ai/multica/server/pkg/agent \
      github.com/multica-ai/multica/server/pkg/agent/internal/testutil
    # CHE-4268: a partition run must also see internal/service, set apart from the rest.
    [ -z "${MULTICA_TEST_GO_WITH_SERVICE:-}" ] || echo github.com/multica-ai/multica/server/internal/service
    ;;
  test)
    printf '%s\n' "$*" >>"$MULTICA_TEST_GO_CALLS"
    ;;
  *)
    echo "unexpected go command: $*" >&2
    exit 2
    ;;
esac
FAKE
chmod 755 "$BIN_DIR/go"

regular_call='test -race -p 1 github.com/multica-ai/multica/server github.com/multica-ai/multica/server/internal/daemon github.com/multica-ai/multica/server/internal/handler'
agent_call='test -race -p 2 -parallel 2 ./pkg/agent/...'

# $1: case label; $2: expected go calls, one per line. Clears the log after.
expect_calls() {
  actual_calls=$(cat "$CALLS_FILE")
  if [ "$actual_calls" != "$2" ]; then
    echo "$1: unexpected go test calls:" >&2
    printf '%s\n' "$actual_calls" >&2
    exit 1
  fi
  : >"$CALLS_FILE"
}

# $1: case label; the rest are arguments for test-go.sh. Asserts the usage
# exit status, the usage line, and that go was never invoked.
expect_usage_failure() {
  label=$1
  shift
  set +e
  PATH="$BIN_DIR:$PATH" bash "$SCRIPT_DIR/test-go.sh" "$@" >"$OUTPUT_FILE" 2>&1
  status=$?
  set -e

  if [ "$status" -ne 2 ]; then
    echo "$label returned status $status, want 2" >&2
    cat "$OUTPUT_FILE" >&2
    exit 1
  fi
  if [ -s "$CALLS_FILE" ]; then
    echo "$label invoked go:" >&2
    cat "$CALLS_FILE" >&2
    exit 1
  fi
  if ! grep -q '^usage: .*test-go.sh \[--race\] \[--only regular|agent\] \[--partition NAME\]$' "$OUTPUT_FILE"; then
    echo "$label did not print usage" >&2
    cat "$OUTPUT_FILE" >&2
    exit 1
  fi
}

PATH="$BIN_DIR:$PATH" bash "$SCRIPT_DIR/test-go.sh" --race
expect_calls "--race" "$regular_call
$agent_call"

PATH="$BIN_DIR:$PATH" bash "$SCRIPT_DIR/test-go.sh" --race --only regular
expect_calls "--only regular" "$regular_call"

# Option order must not matter: CI spells it one way, humans another.
PATH="$BIN_DIR:$PATH" bash "$SCRIPT_DIR/test-go.sh" --only agent --race
expect_calls "--only agent" "$agent_call"

# CHE-2418: per-package databases lift the -p 1 limit. Opt-in, so local and
# release runs keep the serial default.
# internal/handler leaves the shared run and goes to the shard script, which
# gets the same go test flags (a fake here, so the call log shows it).
parallel_call="test -race -p 4 -exec $SCRIPT_DIR/go-test-db-exec.sh github.com/multica-ai/multica/server github.com/multica-ai/multica/server/internal/daemon"
cat >"$TEST_DIR/shards" <<'SHARDS'
#!/usr/bin/env bash
echo "handler-shards $*${MULTICA_HANDLER_SHARD_INDEX:+ index=$MULTICA_HANDLER_SHARD_INDEX}" >>"$MULTICA_TEST_GO_CALLS"
[ -z "${SHARDS_TEST_FAIL:-}" ]
SHARDS
chmod 755 "$TEST_DIR/shards"
export MULTICA_HANDLER_SHARDS_SCRIPT="$TEST_DIR/shards"
MULTICA_TEST_DB_PER_PACKAGE=1 PATH="$BIN_DIR:$PATH" bash "$SCRIPT_DIR/test-go.sh" --race --only regular
actual_calls=$(sort "$CALLS_FILE")
want_calls=$(printf '%s\n' "$parallel_call" "handler-shards -race" | sort)
[ "$actual_calls" = "$want_calls" ] || { echo "MULTICA_TEST_DB_PER_PACKAGE: unexpected calls:" >&2; echo "$actual_calls" >&2; exit 1; }
: >"$CALLS_FILE"

# A failing shard run fails test-go.sh even when the other packages pass.
if SHARDS_TEST_FAIL=1 MULTICA_TEST_DB_PER_PACKAGE=1 PATH="$BIN_DIR:$PATH" bash "$SCRIPT_DIR/test-go.sh" --race --only regular >/dev/null 2>&1; then
  echo "handler shard failure must fail test-go.sh" >&2
  exit 1
fi
: >"$CALLS_FILE"

MULTICA_TEST_DB_PER_PACKAGE=1 PATH="$BIN_DIR:$PATH" bash "$SCRIPT_DIR/test-go.sh" --only agent --race
expect_calls "per-package databases leave pkg/agent alone" "$agent_call"

# CHE-4268: --partition splits the regular half so CI runs it as parallel jobs.
# handler-N = shard N of internal/handler; service = internal/service; rest =
# every other regular package. Together they must equal the unpartitioned run.
partition_call() { echo "test -race -p 4 -exec $SCRIPT_DIR/go-test-db-exec.sh $*"; }
svc=github.com/multica-ai/multica/server/internal/service
base=github.com/multica-ai/multica/server
run_partition() { # $1 partition; later args: extra env assignments
  : >"$CALLS_FILE"
  env MULTICA_TEST_DB_PER_PACKAGE=1 MULTICA_TEST_GO_WITH_SERVICE=1 PATH="$BIN_DIR:$PATH" bash "$SCRIPT_DIR/test-go.sh" --race --only regular --partition "$1"
}
run_partition handler-1
expect_calls "--partition handler-1" "handler-shards -race index=1"
run_partition service
expect_calls "--partition service" "$(partition_call $svc)"
run_partition rest
expect_calls "--partition rest" "$(partition_call $base $base/internal/daemon)"
# Without per-package databases a partition would race shared tables: refuse.
if MULTICA_TEST_GO_WITH_SERVICE=1 PATH="$BIN_DIR:$PATH" bash "$SCRIPT_DIR/test-go.sh" --race --only regular --partition rest >/dev/null 2>&1; then
  echo "--partition without MULTICA_TEST_DB_PER_PACKAGE=1 must fail" >&2; exit 1
fi
: >"$CALLS_FILE"
# A failing handler shard fails its partition.
if SHARDS_TEST_FAIL=1 MULTICA_TEST_DB_PER_PACKAGE=1 PATH="$BIN_DIR:$PATH" bash "$SCRIPT_DIR/test-go.sh" --race --only regular --partition handler-0 >/dev/null 2>&1; then
  echo "handler partition failure must fail test-go.sh" >&2; exit 1
fi
: >"$CALLS_FILE"
expect_usage_failure "unknown partition" --only regular --partition everything
expect_usage_failure "handler partition without index" --only regular --partition handler-
expect_usage_failure "partition with agent half" --only agent --partition rest
expect_usage_failure "missing partition name" --partition
# CHE-4270: merge_group sets MULTICA_TEST_RERUN_FAILS=1. Every run does a first
# pass with no retry; test-go.sh then decides once for the whole job (regular
# packages, handler shards, agent) and reruns the failed tests by name only
# when 1 to 3 tests failed in total and none is a panic or race (plan line `!`).
# Anything else stays on plain `go test`.
# Fake gotestsum: first pass fails the tests in $FAKE_FAIL (space separated
# `<package>:<test>`, empty = pass) with $FAKE_UNSAFE as extra output; a call
# with -run is a rerun and passes (a pass event per test, except those in
# $FAKE_RERUN_SKIP) unless $FAKE_RERUN_FAIL is set; $FAKE_RERUN_NOJSON makes it exit 0 with no JSON file.
cat >"$BIN_DIR/gotestsum" <<'FAKE'
#!/usr/bin/env bash
echo "gotestsum $*" >>"$MULTICA_TEST_GO_CALLS"
json= rerun=
for arg in "$@"; do
  [ "$prev" = --jsonfile ] && json=$arg
  [ "$arg" = -run ] && rerun=1
  prev=$arg
done
: >"$json"
if [ -n "$rerun" ]; then
  [ -z "${FAKE_RERUN_NOJSON:-}" ] || { rm -f "$json"; exit 0; }
  for t in ${FAKE_FAIL:-}; do
    case " ${FAKE_RERUN_SKIP:-} " in *" $t "*) continue ;; esac
    printf '{"Action":"pass","Package":"%s","Test":"%s"}\n' "${t%%:*}" "${t##*:}" >>"$json"
  done
  [ -z "${FAKE_RERUN_FAIL:-}" ]; exit
fi
for t in ${FAKE_FAIL:-}; do
  printf '{"Action":"fail","Package":"%s","Test":"%s"}\n' "${t%%:*}" "${t##*:}" >>"$json"
done
[ -z "${FAKE_UNSAFE:-}" ] || printf '{"Action":"output","Package":"p","Output":"%s\\n"}\n' "$FAKE_UNSAFE" >>"$json"
[ -z "${FAKE_FAIL:-}${FAKE_UNSAFE:-}" ]
FAKE
chmod 755 "$BIN_DIR/gotestsum"
# Fake handler shards script: publishes $FAKE_SHARD_FAIL (plan lines, `\n` separated) like the real one,
# then records the job-wide decision it receives.
cat >"$TEST_DIR/fake-shards" <<'FAKE'
#!/usr/bin/env bash
printf '%b' "${FAKE_SHARD_FAIL:-}" >"$MULTICA_FLAKE_DIR/handler-failed.txt"
touch "$MULTICA_FLAKE_DIR/handler-first-pass.done"
[ -z "${FAKE_SHARD_FAIL:-}" ] && exit 0
until [ -e "$MULTICA_FLAKE_DIR/rerun-decision" ]; do sleep 0.1; done
cp "$MULTICA_FLAKE_DIR/rerun-decision" "$MULTICA_FLAKE_DIR/decision-seen"
[ "$(cat "$MULTICA_FLAKE_DIR/rerun-decision")" = rerun ]
FAKE
chmod 755 "$TEST_DIR/fake-shards"
flake_dir="$TEST_DIR/flakes"
rerun_flags="--format standard-verbose"
reruns() { grep -c -- ' -run ' "$CALLS_FILE" || true; }
# $1 label, $2 want exit (0 = zero), $3 want regular reruns, $4 want decision (empty = none), rest: env
budget_case() {
  local label=$1 want=$2 want_reruns=$3 want_decision=$4 status=0
  shift 4
  rm -rf "$flake_dir"
  : >"$CALLS_FILE"
  env "$@" MULTICA_FLAKE_DIR="$flake_dir" MULTICA_TEST_RERUN_FAILS=1 MULTICA_TEST_DB_PER_PACKAGE=1 \
    MULTICA_HANDLER_SHARDS_SCRIPT="$TEST_DIR/fake-shards" PATH="$BIN_DIR:$PATH" \
    bash "$SCRIPT_DIR/test-go.sh" --only regular >/dev/null 2>&1 || status=$?
  { [ "$want" = 0 ] && [ "$status" -eq 0 ]; } || { [ "$want" != 0 ] && [ "$status" -ne 0 ]; } \
    || { echo "$label: exit $status, want $want" >&2; exit 1; }
  [ "$(reruns)" -eq "$want_reruns" ] || { echo "$label: $(reruns) regular reruns, want $want_reruns" >&2; cat "$CALLS_FILE" >&2; exit 1; }
  if [ -n "$want_decision" ]; then
    [ "$(cat "$flake_dir/decision-seen" 2>/dev/null)" = "$want_decision" ] \
      || { echo "$label: shards saw '$(cat "$flake_dir/decision-seen" 2>/dev/null)', want $want_decision" >&2; exit 1; }
  fi
}
budget_case "no failure" 0 0 "" FAKE_FAIL=
budget_case "one regular failure reruns" 0 1 "" FAKE_FAIL=a:TestA
# 2 regular + 1 shard failure = 3 in total: the shards are told to rerun, and so does the regular run.
budget_case "3 split failures rerun" 0 2 rerun FAKE_FAIL="a:TestA b:TestB" FAKE_SHARD_FAIL='internal/handler\tTestH\tTestH\n'
# 2 regular + 2 shard failures = 4 in total: nobody reruns anything, the job fails.
budget_case "4 split failures never rerun" 1 0 skip FAKE_FAIL="a:TestA b:TestB" FAKE_SHARD_FAIL='internal/handler\tTestH1\tTestH1\ninternal/handler\tTestH2\tTestH2\n'
# 1 regular failure + 3 shard failures (4): the regular failure is not rerun either.
budget_case "regular + 3 shard failures never rerun" 1 0 skip FAKE_FAIL=a:TestA FAKE_SHARD_FAIL='internal/handler\tT1\tT1\ninternal/handler\tT2\tT2\ninternal/handler\tT3\tT3\n'
# 4 failures in the regular packages alone.
budget_case "4 regular failures never rerun" 1 0 "" FAKE_FAIL="a:T1 a:T2 b:T3 b:T4"
# A shard that cannot be rerun (panic, race) vetoes the regular rerun, and a regular panic vetoes the shard rerun.
# Subtests count one by one: four sibling failures under one root never rerun, three rerun once through the root.
budget_case "4 sibling subtests never rerun" 1 0 "" FAKE_FAIL="a:TestA a:TestA/s1 a:TestA/s2 a:TestA/s3 a:TestA/s4"
budget_case "3 sibling subtests rerun" 0 1 "" FAKE_FAIL="a:TestA a:TestA/s1 a:TestA/s2 a:TestA/s3"
budget_case "regular subtest + 3 shard subtests never rerun" 1 0 skip FAKE_FAIL="a:TestA a:TestA/s1" \
  FAKE_SHARD_FAIL='internal/handler\tTestH\tTestH/s1\ninternal/handler\tTestH\tTestH/s2\ninternal/handler\tTestH\tTestH/s3\n'
# A root rerun that skips a failed subtest has not recovered it: the job fails.
budget_case "skipped leaf is not recovered" 1 1 "" FAKE_FAIL="a:TestA a:TestA/s1 a:TestA/s2" FAKE_RERUN_SKIP="a:TestA/s2"
# A rerun that exits 0 but leaves no readable JSON proves nothing: the parser error must fail the job.
budget_case "missing rerun JSON is not recovered" 1 1 "" FAKE_FAIL=a:TestA FAKE_RERUN_NOJSON=1
budget_case "shard veto" 1 0 skip FAKE_FAIL=a:TestA FAKE_SHARD_FAIL='!\n'
budget_case "regular race vetoes shards" 1 0 skip FAKE_FAIL=a:TestA FAKE_UNSAFE='WARNING: DATA RACE' FAKE_SHARD_FAIL='internal/handler\tTestH\tTestH\n'
budget_case "rerun fails again" 1 1 "" FAKE_FAIL=a:TestA FAKE_RERUN_FAIL=1

# First pass and rerun call shapes, agent half (no shards).
: >"$CALLS_FILE"
FAKE_FAIL=a:TestA MULTICA_FLAKE_DIR="$flake_dir" MULTICA_TEST_RERUN_FAILS=1 PATH="$BIN_DIR:$PATH" bash "$SCRIPT_DIR/test-go.sh" --race --only agent
expect_calls "rerun mode agent" "gotestsum $rerun_flags --jsonfile $flake_dir/go-test-agent.json --packages ./pkg/agent/... -- -race -p 2 -parallel 2
gotestsum $rerun_flags --jsonfile $flake_dir/go-test-agent-rerun-1.json --packages a -- -race -p 2 -parallel 2 -count=1 -run ^(TestA)\$"

# CHE-4268 x CHE-4270: a handler-N partition has no regular package, so it never
# runs gotestsum itself. Its decision comes from its own shard only, and a flaky
# shard test is still told to rerun.
partition_rerun_case() { # $1 label, $2 want exit (0 = zero), $3 want decision, rest: env
  local label=$1 want=$2 want_decision=$3 status=0
  shift 3
  rm -rf "$flake_dir"
  : >"$CALLS_FILE"
  env "$@" MULTICA_FLAKE_DIR="$flake_dir" MULTICA_TEST_RERUN_FAILS=1 MULTICA_TEST_DB_PER_PACKAGE=1 \
    MULTICA_HANDLER_SHARDS_SCRIPT="$TEST_DIR/fake-shards" PATH="$BIN_DIR:$PATH" \
    bash "$SCRIPT_DIR/test-go.sh" --only regular --partition handler-2 >/dev/null 2>&1 || status=$?
  { [ "$want" = 0 ] && [ "$status" -eq 0 ]; } || { [ "$want" != 0 ] && [ "$status" -ne 0 ]; } \
    || { echo "$label: exit $status, want $want" >&2; exit 1; }
  [ ! -s "$CALLS_FILE" ] || { echo "$label: a handler partition must not run gotestsum:" >&2; cat "$CALLS_FILE" >&2; exit 1; }
  [ "$(cat "$flake_dir/decision-seen" 2>/dev/null)" = "$want_decision" ] \
    || { echo "$label: shard saw '$(cat "$flake_dir/decision-seen" 2>/dev/null)', want '$want_decision'" >&2; exit 1; }
}
partition_rerun_case "partition: shard passes" 0 ""
partition_rerun_case "partition: one shard failure reruns" 0 rerun FAKE_SHARD_FAIL='internal/handler\tTestH\tTestH\n'
partition_rerun_case "partition: four shard failures never rerun" 1 skip \
  FAKE_SHARD_FAIL='internal/handler\tT1\tT1\ninternal/handler\tT2\tT2\ninternal/handler\tT3\tT3\ninternal/handler\tT4\tT4\n'

# Any value but 1 is off.
: >"$CALLS_FILE"
MULTICA_TEST_RERUN_FAILS=0 PATH="$BIN_DIR:$PATH" bash "$SCRIPT_DIR/test-go.sh" --only agent --race
expect_calls "rerun off" "$agent_call"

expect_usage_failure "unknown option" --unknown
expect_usage_failure "unknown --only scope" --only everything
expect_usage_failure "missing --only scope" --only

echo "test-go.test.sh: PASS"
