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
# CHE-4270: with MULTICA_TEST_RERUN_FAILS=1 (merge_group) a shard that fails on
# 1-3 named top-level tests reruns just those once. Pass on rerun = flake,
# recorded in $MULTICA_FLAKE_DIR/handler-flakes.txt; fail again = shard fails.
# The fake binary fails its first call with $FIRST_OUT / $FIRST_EXIT; a call
# whose -test.run equals $RERUN_RE exits $RERUN_EXIT; everything else passes.
cat >"$TEST_DIR/bin/go" <<'FAKE'
#!/usr/bin/env bash
set -eu
out=
while [ "$#" -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done
cat >"$out" <<'BIN'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$SHARD_TEST_DIR/runs"
case " $* " in
  *" -test.run $RERUN_RE "*) exit "${RERUN_EXIT:-0}" ;;
esac
if ! [ -e "$SHARD_TEST_DIR/failed-once" ] && mkdir "$SHARD_TEST_DIR/failed-once" 2>/dev/null; then
  printf '%s\n' "$FIRST_OUT"; exit "${FIRST_EXIT:-1}"
fi
exit 0
BIN
chmod 755 "$out"
FAKE
rerun_case() { # $1 label, $2 want exit, $3 want rerun count; rest: env from caller
  rm -rf "$TEST_DIR/flakes" "$TEST_DIR/failed-once"
  : >"$TEST_DIR/runs"
  local status=0
  MULTICA_TEST_RERUN_FAILS=1 MULTICA_FLAKE_DIR="$TEST_DIR/flakes" PATH="$TEST_DIR/bin:$PATH" \
    MULTICA_TEST_EXEC="$TEST_DIR/exec" MULTICA_HANDLER_SHARDS=3 \
    bash "$SCRIPT_DIR/go-test-handler-shards.sh" >/dev/null 2>&1 || status=$?
  [ "$status" -eq "$2" ] || { echo "$1: exit $status, want $2" >&2; exit 1; }
  local reruns
  reruns=$(grep -cF -- "-test.run $RERUN_RE " "$TEST_DIR/runs" || true)
  [ "$reruns" -eq "$3" ] || { echo "$1: $reruns reruns, want $3" >&2; exit 1; }
}
export SHARD_TEST_DIR
export RERUN_RE='^(TestFlakyA|TestFlakyB)$'
export FIRST_OUT=$'--- FAIL: TestFlakyB (0.10s)\n    --- FAIL: TestFlakyB/sub (0.10s)\n--- FAIL: TestFlakyA (0.20s)\nFAIL'

# Fails once, passes on rerun: shard passes, both tests recorded as flakes.
RERUN_EXIT=0 rerun_case "flake" 0 1
[ "$(sort "$TEST_DIR/flakes/handler-flakes.txt")" = "$(printf 'internal/handler\tTestFlakyA\ninternal/handler\tTestFlakyB')" ] \
  || { echo "flake file wrong:" >&2; cat "$TEST_DIR/flakes/handler-flakes.txt" >&2; exit 1; }

# Fails again on rerun: real failure, nothing recorded.
RERUN_EXIT=1 rerun_case "fails twice" 1 1
[ ! -s "$TEST_DIR/flakes/handler-flakes.txt" ] || { echo "no flake may be recorded when the rerun fails" >&2; exit 1; }

# More than 3 failing tests: a real break, no rerun.
FIRST_OUT=$'--- FAIL: TestA (0s)\n--- FAIL: TestB (0s)\n--- FAIL: TestC (0s)\n--- FAIL: TestD (0s)\nFAIL' RERUN_EXIT=0 rerun_case ">3 failures" 1 0

# A panic or timeout leaves later tests unrun, so a rerun of the named ones proves nothing.
FIRST_OUT=$'--- FAIL: TestFlakyA (0s)\npanic: test timed out after 10m0s' FIRST_EXIT=2 RERUN_EXIT=0 rerun_case "panic" 1 0

# No named failure (TestMain, build): nothing to rerun.
FIRST_OUT='FAIL' RERUN_EXIT=0 rerun_case "no names" 1 0

# Default (pull_request, push): no rerun, failure shows.
rm -rf "$TEST_DIR/failed-once"
: >"$TEST_DIR/runs"
status=0
RERUN_EXIT=0 PATH="$TEST_DIR/bin:$PATH" MULTICA_TEST_EXEC="$TEST_DIR/exec" MULTICA_HANDLER_SHARDS=3 \
  bash "$SCRIPT_DIR/go-test-handler-shards.sh" >/dev/null 2>&1 || status=$?
[ "$status" -eq 1 ] || { echo "rerun off: exit $status, want 1" >&2; exit 1; }
! grep -qF -- "-test.run $RERUN_RE " "$TEST_DIR/runs" || { echo "rerun off must not rerun" >&2; exit 1; }
echo "go-test-handler-shards.test.sh: PASS"
