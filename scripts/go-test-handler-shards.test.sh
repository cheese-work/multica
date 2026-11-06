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
