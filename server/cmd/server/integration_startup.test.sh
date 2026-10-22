#!/usr/bin/env bash
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SERVER_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
TEST_DIR=$(mktemp -d "${TMPDIR:-/tmp}/multica-integration-startup.XXXXXX")
trap 'rm -rf "$TEST_DIR"' EXIT

cd "$SERVER_ROOT"
go test -c -o "$TEST_DIR/server.test" ./cmd/server

for database_case in unreachable invalid; do
  case "$database_case" in
    unreachable) database_url="postgres://multica:multica@/multica_test?host=$TEST_DIR/missing-postgres&connect_timeout=1&sslmode=disable" ;;
    invalid) database_url='postgres://multica:multica@localhost:invalid/multica_test' ;;
  esac

  for ci_value in true false unset; do
    set +e
    if [ "$ci_value" = unset ]; then
      env -u CI DATABASE_URL="$database_url" "$TEST_DIR/server.test" -test.run '^$' >"$TEST_DIR/output" 2>&1
    else
      CI="$ci_value" DATABASE_URL="$database_url" "$TEST_DIR/server.test" -test.run '^$' >"$TEST_DIR/output" 2>&1
    fi
    status=$?
    set -e

    if [ "$ci_value" = true ]; then
      expected_status=1
      expected_message='ERROR: integration suite did not run: database is required in CI:'
    else
      expected_status=0
      expected_message='WARNING: integration suite did not run: database unavailable:'
    fi

    cat "$TEST_DIR/output"
    if [ "$status" -ne "$expected_status" ]; then
      echo "FAIL: CI=$ci_value, $database_case database: exit $status, want $expected_status" >&2
      exit 1
    fi
    if ! grep -Fq "$expected_message" "$TEST_DIR/output"; then
      echo "FAIL: CI=$ci_value, $database_case database: missing diagnostic: $expected_message" >&2
      exit 1
    fi
    echo "PASS: CI=$ci_value, $database_case database: exit $status"
  done
done
