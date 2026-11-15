#!/usr/bin/env bash
set -eu

# go-test-handler-shards.sh (CHE-2418 B1): internal/handler is one package, so
# `go test -p` ran it in one process and it set the backend Test step's floor.
# The shards must (1) build the test binary once, (2) cover every top-level
# test exactly once, (3) fail if any shard fails. go and the exec wrapper are
# faked; the fake binary records the -test.run regex it was given.

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
TEST_DIR=$(mktemp -d "${TMPDIR:-/tmp}/multica-handler-shards.XXXXXX")
trap 'rm -rf "$TEST_DIR"' EXIT
mkdir -p "$TEST_DIR/bin"
export SHARD_TEST_DIR="$TEST_DIR"

cat >"$TEST_DIR/bin/go" <<'FAKE'
#!/usr/bin/env bash
# go test -c [flags] -o <out> <pkg>: write a "test binary" that logs its args.
set -eu
[ "$1" = test ] && [ "$2" = -c ] || { echo "unexpected go call: $*" >&2; exit 2; }
echo "$*" >>"$SHARD_TEST_DIR/builds"
out=
while [ "$#" -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done
cat >"$out" <<'BIN'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$SHARD_TEST_DIR/runs"
exit "${SHARD_TEST_EXIT:-0}"
BIN
chmod 755 "$out"
FAKE
chmod 755 "$TEST_DIR/bin/go"
printf '#!/usr/bin/env bash\nexec "$@"\n' >"$TEST_DIR/exec"
chmod 755 "$TEST_DIR/exec"

run_shards() {
  PATH="$TEST_DIR/bin:$PATH" MULTICA_TEST_EXEC="$TEST_DIR/exec" MULTICA_HANDLER_SHARDS=3 \
    bash "$SCRIPT_DIR/go-test-handler-shards.sh" "$@"
}

run_shards -race
[ "$(wc -l <"$TEST_DIR/builds")" -eq 1 ] || { echo "want exactly one build" >&2; exit 1; }
grep -q -- '-race' "$TEST_DIR/builds" || { echo "build lost the -race flag" >&2; exit 1; }
[ "$(wc -l <"$TEST_DIR/runs")" -eq 3 ] || { echo "want 3 shard runs" >&2; exit 1; }

# Every top-level test in the package appears in exactly one shard's regex.
expected=$(grep -hoE '^func (Test|Example|Fuzz)[A-Za-z0-9_]*\(' "$REPO_ROOT"/server/internal/handler/*_test.go \
  | sed -E 's/^func ([^(]*)\(/\1/' | sort -u)
covered=$(sed -E 's/.*-test\.run ([^ ]+).*/\1/' "$TEST_DIR/runs" | sed -E 's/^\^\((.*)\)\$$/\1/' | tr '|' '\n' | sort)
[ "$covered" = "$expected" ] || { echo "shards do not cover each test exactly once" >&2; exit 1; }
[ -n "$expected" ] || { echo "found no tests to shard" >&2; exit 1; }

# One failing shard fails the script.
: >"$TEST_DIR/runs"
if SHARD_TEST_EXIT=1 run_shards >/dev/null 2>&1; then echo "failing shards must fail the script" >&2; exit 1; fi
# A shard count that is not an integer >= 1 would start no process and still
# exit 0 while test-go.sh has already dropped internal/handler from its run.
for bad in 0 -1 abc; do
  : >"$TEST_DIR/builds"
  : >"$TEST_DIR/runs"
  if MULTICA_HANDLER_SHARDS=$bad PATH="$TEST_DIR/bin:$PATH" MULTICA_TEST_EXEC="$TEST_DIR/exec" \
    bash "$SCRIPT_DIR/go-test-handler-shards.sh" >/dev/null 2>&1; then
    echo "MULTICA_HANDLER_SHARDS=$bad must fail the script" >&2; exit 1
  fi
  [ ! -s "$TEST_DIR/builds" ] || { echo "MULTICA_HANDLER_SHARDS=$bad must fail before compiling" >&2; exit 1; }
done

# CHE-4268: MULTICA_HANDLER_SHARD_INDEX runs one shard (a CI job each). Across
# all indexes the shards still cover every test exactly once, with one build each.
: >"$TEST_DIR/runs"
for i in 0 1 2; do
  : >"$TEST_DIR/builds"
  MULTICA_HANDLER_SHARD_INDEX=$i run_shards -race
  [ "$(wc -l <"$TEST_DIR/builds")" -eq 1 ] || { echo "index $i: want exactly one build" >&2; exit 1; }
done
[ "$(wc -l <"$TEST_DIR/runs")" -eq 3 ] || { echo "indexed runs: want 3 shard runs in total" >&2; exit 1; }
covered=$(sed -E 's/.*-test\.run ([^ ]+).*/\1/' "$TEST_DIR/runs" | sed -E 's/^\^\((.*)\)\$$/\1/' | tr '|' '\n' | sort)
[ "$covered" = "$expected" ] || { echo "indexed shards do not cover each test exactly once" >&2; exit 1; }
# An index outside 0..shards-1 would run nothing and still pass: fail before compiling.
for bad in 3 -1 abc 01; do
  : >"$TEST_DIR/builds"
  : >"$TEST_DIR/runs"
  if MULTICA_HANDLER_SHARD_INDEX=$bad run_shards >/dev/null 2>&1; then
    echo "MULTICA_HANDLER_SHARD_INDEX=$bad must fail the script" >&2; exit 1
  fi
  [ ! -s "$TEST_DIR/builds" ] || { echo "MULTICA_HANDLER_SHARD_INDEX=$bad must fail before compiling" >&2; exit 1; }
done
# A failing indexed shard fails the script.
if SHARD_TEST_EXIT=1 MULTICA_HANDLER_SHARD_INDEX=1 run_shards >/dev/null 2>&1; then echo "failing indexed shard must fail the script" >&2; exit 1; fi

# CHE-4270: with MULTICA_TEST_RERUN_FAILS=1 (merge_group) the shards run a first
# pass, publish their failures in $MULTICA_FLAKE_DIR/handler-failed.txt (plan
# lines `internal/handler<TAB>Test`, or `!` when a rerun proves nothing), touch
# handler-first-pass.done and wait for test-go.sh's job-wide `rerun-decision`.
# `rerun`: the failed tests run once more; a pass is a flake recorded in
# handler-flakes.txt. `skip` (or no decision in time): the shard script fails.
# The fake binary fails its first call with $FIRST_OUT / $FIRST_EXIT and, when
# set, its second call with $SECOND_OUT; a call whose -test.run equals $RERUN_RE
# prints $RERUN_OUT and exits $RERUN_EXIT; everything else passes.
cat >"$TEST_DIR/bin/go" <<'FAKE'
#!/usr/bin/env bash
set -eu
out=
while [ "$#" -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done
cat >"$out" <<'BIN'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$SHARD_TEST_DIR/runs"
case " $* " in
  *" -test.run $RERUN_RE "*) printf '%s\n' "${RERUN_OUT:-}"; exit "${RERUN_EXIT:-0}" ;;
esac
if ! [ -e "$SHARD_TEST_DIR/failed-1" ] && mkdir "$SHARD_TEST_DIR/failed-1" 2>/dev/null; then
  printf '%s\n' "$FIRST_OUT"; exit "${FIRST_EXIT:-1}"
fi
if [ -n "${SECOND_OUT:-}" ] && ! [ -e "$SHARD_TEST_DIR/failed-2" ] && mkdir "$SHARD_TEST_DIR/failed-2" 2>/dev/null; then
  printf '%s\n' "$SECOND_OUT"; exit 1
fi
exit 0
BIN
chmod 755 "$out"
FAKE
# $1 label, $2 want exit, $3 want rerun count, $4 decision to hand over ("" = none, with a 2 s wait limit)
rerun_case() {
  rm -rf "$TEST_DIR/flakes" "$TEST_DIR/failed-1" "$TEST_DIR/failed-2"
  mkdir -p "$TEST_DIR/flakes"
  : >"$TEST_DIR/runs"
  [ -z "$4" ] || echo "$4" >"$TEST_DIR/flakes/rerun-decision"
  local status=0
  MULTICA_TEST_RERUN_FAILS=1 MULTICA_FLAKE_DIR="$TEST_DIR/flakes" MULTICA_RERUN_WAIT=2 PATH="$TEST_DIR/bin:$PATH" \
    MULTICA_TEST_EXEC="$TEST_DIR/exec" MULTICA_HANDLER_SHARDS=3 \
    bash "$SCRIPT_DIR/go-test-handler-shards.sh" >/dev/null 2>&1 || status=$?
  [ "$status" -eq "$2" ] || { echo "$1: exit $status, want $2" >&2; exit 1; }
  local reruns
  reruns=$(grep -cF -- "-test.run $RERUN_RE " "$TEST_DIR/runs" || true)
  [ "$reruns" -eq "$3" ] || { echo "$1: $reruns reruns, want $3" >&2; exit 1; }
}
published() { sort "$TEST_DIR/flakes/handler-failed.txt" | paste -sd, | tr '\t' ':'; }
export SHARD_TEST_DIR
export RERUN_RE='^(TestFlakyA|TestFlakyB)$'
export FIRST_OUT=$'--- FAIL: TestFlakyB (0.10s)\n    --- FAIL: TestFlakyB/sub (0.10s)\n--- FAIL: TestFlakyA (0.20s)\nFAIL'

# Fails once, decision rerun, passes on rerun: shard script passes, both tests recorded as flakes.
export RERUN_OUT=$'--- PASS: TestFlakyA (0s)\n--- PASS: TestFlakyB (0s)\n    --- PASS: TestFlakyB/sub (0s)'
RERUN_EXIT=0 rerun_case "flake" 0 1 rerun
[ "$(published)" = "internal/handler:TestFlakyA:TestFlakyA,internal/handler:TestFlakyB:TestFlakyB/sub" ] || { echo "published wrong: $(published)" >&2; exit 1; }
[ -e "$TEST_DIR/flakes/handler-first-pass.done" ] || { echo "first-pass marker missing" >&2; exit 1; }
[ "$(sort "$TEST_DIR/flakes/handler-flakes.txt")" = "$(printf 'internal/handler\tTestFlakyA\ninternal/handler\tTestFlakyB/sub')" ] \
  || { echo "flake file wrong:" >&2; cat "$TEST_DIR/flakes/handler-flakes.txt" >&2; exit 1; }

# The root reruns and passes, but a failed subtest was skipped: not recovered, nothing recorded.
RERUN_OUT=$'--- PASS: TestFlakyA (0s)\n--- PASS: TestFlakyB (0s)\n    --- SKIP: TestFlakyB/sub (0s)' RERUN_EXIT=0 rerun_case "skipped leaf" 1 1 rerun
[ ! -s "$TEST_DIR/flakes/handler-flakes.txt" ] || { echo "a skipped leaf must not be recorded as a flake" >&2; exit 1; }

# Fails again on rerun: real failure, nothing recorded.
RERUN_EXIT=1 rerun_case "fails twice" 1 1 rerun
[ ! -s "$TEST_DIR/flakes/handler-flakes.txt" ] || { echo "no flake may be recorded when the rerun fails" >&2; exit 1; }

# The job-wide decision says skip (e.g. other runs also failed): publish, never rerun, fail.
RERUN_EXIT=0 rerun_case "decision skip" 1 0 skip
[ "$(published)" = "internal/handler:TestFlakyA:TestFlakyA,internal/handler:TestFlakyB:TestFlakyB/sub" ] || { echo "skip: published wrong: $(published)" >&2; exit 1; }

# No decision arrives: fail instead of hanging or guessing.
RERUN_EXIT=0 rerun_case "no decision" 1 0 ""

# Four sibling subtests of one test are four failures: published one by one, rerun through their root.
FIRST_OUT=$'--- FAIL: TestFlakyA (0s)\n    --- FAIL: TestFlakyA/s1 (0s)\n    --- FAIL: TestFlakyA/s2 (0s)\n    --- FAIL: TestFlakyA/s3 (0s)\n    --- FAIL: TestFlakyA/s4 (0s)\nFAIL' \
  RERUN_RE='^(TestFlakyA)$' RERUN_EXIT=0 rerun_case "sibling subtests" 1 0 skip
[ "$(published | tr ',' '\n' | wc -l | tr -d ' ')" = 4 ] || { echo "sibling subtests: want 4 published leaves, got $(published)" >&2; exit 1; }
FIRST_OUT=$'--- FAIL: TestFlakyA (0s)\n    --- FAIL: TestFlakyA/s1 (0s)\n    --- FAIL: TestFlakyA/s2 (0s)\nFAIL' \
  RERUN_OUT=$'--- PASS: TestFlakyA (0s)\n    --- PASS: TestFlakyA/s1 (0s)\n    --- PASS: TestFlakyA/s2 (0s)' \
  RERUN_RE='^(TestFlakyA)$' RERUN_EXIT=0 rerun_case "sibling subtests rerun by root" 0 1 rerun
[ "$(sort "$TEST_DIR/flakes/handler-flakes.txt" | paste -sd, | tr '\t' ':')" = "internal/handler:TestFlakyA/s1,internal/handler:TestFlakyA/s2" ] \
  || { echo "sibling flakes keep their own names:" >&2; cat "$TEST_DIR/flakes/handler-flakes.txt" >&2; exit 1; }

# Failures in two shards are published together, so the job-wide count sees all of them.
SECOND_OUT=$'--- FAIL: TestOther1 (0s)\n--- FAIL: TestOther2 (0s)\nFAIL' RERUN_EXIT=0 rerun_case "two shards" 1 0 skip
[ "$(published | tr ',' '\n' | wc -l | tr -d ' ')" = 4 ] || { echo "two shards: want 4 published failures, got $(published)" >&2; exit 1; }

# A panic or timeout leaves later tests unrun, so a rerun of the named ones proves nothing: `!`.
FIRST_OUT=$'--- FAIL: TestFlakyA (0s)\npanic: test timed out after 10m0s' FIRST_EXIT=2 RERUN_EXIT=0 rerun_case "panic" 1 0 skip
[ "$(published)" = '!' ] || { echo "panic: want '!', got $(published)" >&2; exit 1; }

# A race report fails the test with exit 1 and a named --- FAIL. A passing rerun
# would hide it, so a race is `!` even if the decision says rerun.
FIRST_OUT=$'WARNING: DATA RACE\n--- FAIL: TestFlakyA (0s)\n    testing.go:1: race detected during execution of test\nFAIL' RERUN_EXIT=0 rerun_case "data race" 1 0 skip
[ "$(published)" = '!' ] || { echo "data race: want '!', got $(published)" >&2; exit 1; }

# No named failure (TestMain, build): `!`.
FIRST_OUT='FAIL' RERUN_EXIT=0 rerun_case "no names" 1 0 skip
[ "$(published)" = '!' ] || { echo "no names: want '!', got $(published)" >&2; exit 1; }

# Nothing fails: no wait, no rerun, exit 0.
FIRST_EXIT=0 FIRST_OUT=ok RERUN_EXIT=0 rerun_case "all pass" 0 0 ""
[ ! -s "$TEST_DIR/flakes/handler-failed.txt" ] || { echo "all pass: nothing may be published" >&2; exit 1; }

# Default (pull_request, push): no rerun, failure shows.
rm -rf "$TEST_DIR/failed-1" "$TEST_DIR/failed-2"
: >"$TEST_DIR/runs"
status=0
RERUN_EXIT=0 PATH="$TEST_DIR/bin:$PATH" MULTICA_TEST_EXEC="$TEST_DIR/exec" MULTICA_HANDLER_SHARDS=3 \
  bash "$SCRIPT_DIR/go-test-handler-shards.sh" >/dev/null 2>&1 || status=$?
[ "$status" -eq 1 ] || { echo "rerun off: exit $status, want 1" >&2; exit 1; }
! grep -qF -- "-test.run $RERUN_RE " "$TEST_DIR/runs" || { echo "rerun off must not rerun" >&2; exit 1; }
# CHE-4243: each shard runs ~5-8 min on a healthy runner but ~10+ min on a slow
# one, so the per-shard -test.timeout is 20m by default and overridable. It
# must reach every shard, and a malformed value must fail before compiling.
: >"$TEST_DIR/runs"
run_shards >/dev/null
[ "$(grep -c -- '-test.timeout=20m' "$TEST_DIR/runs")" -eq 3 ] || { echo "want -test.timeout=20m on all 3 shards by default" >&2; exit 1; }
: >"$TEST_DIR/runs"
MULTICA_HANDLER_SHARD_TIMEOUT=45m run_shards >/dev/null
[ "$(grep -c -- '-test.timeout=45m' "$TEST_DIR/runs")" -eq 3 ] || { echo "MULTICA_HANDLER_SHARD_TIMEOUT must reach every shard" >&2; exit 1; }
for bad in 0 abc 10 -5m 1h30m; do
  : >"$TEST_DIR/builds"
  if MULTICA_HANDLER_SHARD_TIMEOUT=$bad run_shards >/dev/null 2>&1; then
    echo "MULTICA_HANDLER_SHARD_TIMEOUT=$bad must fail the script" >&2; exit 1
  fi
  [ ! -s "$TEST_DIR/builds" ] || { echo "MULTICA_HANDLER_SHARD_TIMEOUT=$bad must fail before compiling" >&2; exit 1; }
done
echo "go-test-handler-shards.test.sh: PASS"
