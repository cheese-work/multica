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
  if ! grep -q '^usage: .*test-go.sh \[--race\] \[--only regular|agent\]$' "$OUTPUT_FILE"; then
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
echo "handler-shards $*" >>"$MULTICA_TEST_GO_CALLS"
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

expect_usage_failure "unknown option" --unknown
expect_usage_failure "unknown --only scope" --only everything
expect_usage_failure "missing --only scope" --only

echo "test-go.test.sh: PASS"
