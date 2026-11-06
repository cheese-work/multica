#!/usr/bin/env bash
set -eu

# CHE-4270: end to end, with the real gotestsum. test-go.sh runs a fixture Go
# module in merge_group mode: a test that fails once then passes must pass the
# run and be recorded as one flake; a test that fails twice, or more than 3
# failures at once, must fail it. Needs gotestsum on PATH (CI installs it in
# merge_group runs, where this test runs).

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
command -v gotestsum >/dev/null || { echo "gotestsum not on PATH" >&2; exit 1; }
WORK=$(mktemp -d "${TMPDIR:-/tmp}/multica-test-go-rerun.XXXXXX")
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/scripts" "$WORK/server"
cp "$SCRIPT_DIR/test-go.sh" "$SCRIPT_DIR/go-test-flakes.mjs" "$SCRIPT_DIR/go-test-with-agent-cli-guard.sh" "$SCRIPT_DIR/agent-cli-command-names.txt" "$WORK/scripts/"
printf 'module fx\n\ngo 1.21\n' >"$WORK/server/go.mod"
cat >"$WORK/server/fx_test.go" <<'GO'
package fx

import (
	"os"
	"testing"
)

// Each test appends to $FX_DIR/runs.<name> on every execution.
func run(t *testing.T, name string) int {
	f, err := os.OpenFile(os.Getenv("FX_DIR")+"/runs."+name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("x")
	f.Close()
	b, _ := os.ReadFile(os.Getenv("FX_DIR") + "/runs." + name)
	return len(b)
}

func TestFlakyOnce(t *testing.T) {
	if os.Getenv("FX_MODE") == "flaky" && run(t, "flaky") == 1 {
		t.Fatal("first run fails")
	}
}

func TestAlwaysFails(t *testing.T) {
	if os.Getenv("FX_MODE") == "twice" {
		run(t, "twice")
		t.Fatal("always")
	}
}

func TestMany1(t *testing.T) { many(t, "m1") }
func TestMany2(t *testing.T) { many(t, "m2") }
func TestMany3(t *testing.T) { many(t, "m3") }
func TestMany4(t *testing.T) { many(t, "m4") }

func many(t *testing.T, name string) {
	if os.Getenv("FX_MODE") == "many" {
		run(t, name)
		t.Fatal("break")
	}
}
GO

# $1 mode, $2 want exit (0 = zero, else nonzero), rest: extra env (NAME=value)
fx() {
  local mode=$1 want=$2 status=0
  shift 2
  export FX_DIR="$WORK/out-$mode"
  rm -rf "$FX_DIR"
  mkdir -p "$FX_DIR"
  env FX_MODE="$mode" MULTICA_FLAKE_DIR="$FX_DIR/flakes" "$@" bash "$WORK/scripts/test-go.sh" --only regular >"$FX_DIR/log" 2>&1 || status=$?
  if { [ "$want" = 0 ] && [ "$status" -ne 0 ]; } || { [ "$want" != 0 ] && [ "$status" -eq 0 ]; }; then
    echo "$mode: exit $status, want $want" >&2
    cat "$FX_DIR/log" >&2
    exit 1
  fi
}
runs() { wc -c <"$FX_DIR/runs.$1" | tr -d ' '; }

# Fails once, passes on rerun: green, one flake.
fx flaky 0 MULTICA_TEST_RERUN_FAILS=1
[ "$(runs flaky)" = 2 ] || { echo "flaky: want 2 runs, got $(runs flaky)" >&2; exit 1; }
summary=$(node "$SCRIPT_DIR/go-test-flakes.mjs" "$FX_DIR/flakes")
[ "$(node -p "JSON.stringify(require('$FX_DIR/flakes/flakes.json').flakes)")" = '[{"package":"fx","test":"TestFlakyOnce"}]' ] \
  || { echo "flaky: want exactly TestFlakyOnce in flakes.json" >&2; echo "$summary" >&2; exit 1; }

# Fails twice: red, and it ran exactly twice (one rerun, not a retry loop).
fx twice 1 MULTICA_TEST_RERUN_FAILS=1
[ "$(runs twice)" = 2 ] || { echo "twice: want 2 runs, got $(runs twice)" >&2; exit 1; }

# More than 3 failing tests: a real break, no rerun.
fx many 1 MULTICA_TEST_RERUN_FAILS=1
for n in m1 m2 m3 m4; do [ "$(runs $n)" = 1 ] || { echo "many: $n ran $(runs $n) times, want 1" >&2; exit 1; }; done

# Rerun off (pull_request, push): the flake shows as a failure.
fx flaky 1
[ "$(runs flaky)" = 1 ] || { echo "rerun off: want 1 run, got $(runs flaky)" >&2; exit 1; }

echo "test-go-rerun.test.sh: PASS"
