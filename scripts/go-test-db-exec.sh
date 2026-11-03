#!/usr/bin/env bash
# `go test -exec` wrapper (CHE-2418). Go test packages share one Postgres
# database and one Redis DB, which is why test-go.sh used -p 1. With
# MULTICA_TEST_DB_PER_PACKAGE=1 each test binary instead gets its own database
# cloned from the migrated template named by DATABASE_URL, plus its own Redis
# DB index, so packages can run in parallel. Without it, the binary just runs.
# Usage: go test -exec scripts/go-test-db-exec.sh ...
set -u

if [ "${MULTICA_TEST_DB_PER_PACKAGE:-}" != 1 ]; then
  exec "$@"
fi

base=${DATABASE_URL%%\?*}
query=${DATABASE_URL#"$base"}
admin="${base%/*}/postgres$query"
# Test suites refuse databases whose name does not end in _test.
template=${base##*/}
name="${template%_test}_$$_${RANDOM}_test"

psql "$admin" -qv ON_ERROR_STOP=1 -c "CREATE DATABASE \"$name\" TEMPLATE \"$template\"" || exit 1
cleanup() {
  psql "$admin" -qc "DROP DATABASE IF EXISTS \"$name\" WITH (FORCE)" >/dev/null 2>&1
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

export DATABASE_URL="${base%/*}/$name$query"

# Redis tests FlushDB, so concurrent packages need distinct DB indexes. Hold a
# non-blocking flock on the first free index of 1..15 for the run.
if [ -n "${REDIS_TEST_URL:-}" ]; then
  redis_base=${REDIS_TEST_URL%%\?*}
  redis_query=${REDIS_TEST_URL#"$redis_base"}
  redis_host=${redis_base%/*}
  for index in $(seq 1 15); do
    exec {lock_fd}>"${TMPDIR:-/tmp}/multica-test-redis-db-$index.lock"
    if flock -n "$lock_fd"; then
      export REDIS_TEST_URL="$redis_host/$index$redis_query"
      break
    fi
    exec {lock_fd}>&-
  done
fi

"$@"
exit $?
