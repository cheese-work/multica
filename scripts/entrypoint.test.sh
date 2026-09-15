#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
ENTRYPOINT_PID=""

cleanup() {
  if [ -n "$ENTRYPOINT_PID" ]; then
    kill -KILL "$ENTRYPOINT_PID" 2>/dev/null || true
  fi
  if [ -s "$TEST_DIR/migrate.pid" ]; then
    kill -KILL "$(cat "$TEST_DIR/migrate.pid")" 2>/dev/null || true
  fi
  rm -rf "$TEST_DIR"
}
trap cleanup EXIT

cp "$ROOT_DIR/docker/entrypoint.sh" "$TEST_DIR/entrypoint.sh"

cat >"$TEST_DIR/migrate" <<'SCRIPT'
#!/bin/sh
trap 'printf "TERM\n" >"$MIGRATE_SIGNAL_FILE"; exit 143' TERM
printf '%s\n' "$$" >"$MIGRATE_PID_FILE"
printf '%s\n' "$MULTICA_INTERNAL_DATABASE_STARTUP_STARTED_AT_UNIX" >"$MIGRATE_STARTED_AT_FILE"
while :; do
  sleep 1
done
SCRIPT

cat >"$TEST_DIR/server" <<'SCRIPT'
#!/bin/sh
printf 'started\n' >"$SERVER_STARTED_FILE"
printf '%s\n' "$MULTICA_INTERNAL_DATABASE_STARTUP_STARTED_AT_UNIX" >"$SERVER_STARTED_AT_FILE"
SCRIPT

chmod +x "$TEST_DIR/entrypoint.sh" "$TEST_DIR/migrate" "$TEST_DIR/server"

export MIGRATE_PID_FILE="$TEST_DIR/migrate.pid"
export MIGRATE_SIGNAL_FILE="$TEST_DIR/migrate.signal"
export MIGRATE_STARTED_AT_FILE="$TEST_DIR/migrate.started-at"
export SERVER_STARTED_FILE="$TEST_DIR/server.started"
export SERVER_STARTED_AT_FILE="$TEST_DIR/server.started-at"

(
  cd "$TEST_DIR"
  exec ./entrypoint.sh
) &
ENTRYPOINT_PID=$!

for _ in $(seq 1 50); do
  if [ -s "$MIGRATE_PID_FILE" ]; then
    break
  fi
  sleep 0.1
done
if [ ! -s "$MIGRATE_PID_FILE" ]; then
  echo "migrator did not start"
  exit 1
fi

kill -TERM "$ENTRYPOINT_PID"
set +e
wait "$ENTRYPOINT_PID"
status=$?
set -e
ENTRYPOINT_PID=""

if [ "$status" -ne 143 ]; then
  echo "entrypoint exited with $status, want 143 after SIGTERM"
  exit 1
fi
if [ "$(cat "$MIGRATE_SIGNAL_FILE" 2>/dev/null || true)" != "TERM" ]; then
  echo "entrypoint did not forward SIGTERM to the migrator"
  exit 1
fi
if [ -e "$SERVER_STARTED_FILE" ]; then
  echo "entrypoint started the server after interrupted migrations"
  exit 1
fi

cat >"$TEST_DIR/migrate" <<'SCRIPT'
#!/bin/sh
printf '%s\n' "$MULTICA_INTERNAL_DATABASE_STARTUP_STARTED_AT_UNIX" >"$MIGRATE_STARTED_AT_FILE"
exit 0
SCRIPT
chmod +x "$TEST_DIR/migrate"

(
  cd "$TEST_DIR"
  ./entrypoint.sh
)
if [ "$(cat "$SERVER_STARTED_FILE" 2>/dev/null || true)" != "started" ]; then
  echo "entrypoint did not start the server after successful migrations"
  exit 1
fi
started_at="$(cat "$MIGRATE_STARTED_AT_FILE" 2>/dev/null || true)"
if [ -z "$started_at" ] || [ "$started_at" != "$(cat "$SERVER_STARTED_AT_FILE" 2>/dev/null || true)" ]; then
  echo "entrypoint did not share one database startup timestamp"
  exit 1
fi
case "$started_at" in
  *[!0-9]*)
    echo "entrypoint database startup timestamp is not a Unix timestamp"
    exit 1
    ;;
esac

echo "entrypoint startup and signal forwarding ok"

# --- MULTICA_SKIP_MIGRATIONS guard (CHE-530) ---
#
# The successful-migration run above already covers the guard's default
# (unset) path: migrate ran and its started-at marker was written. These two
# runs cover the rest of the matrix the guard's own header comment promises:
# unset/absent still runs migrations (repeated explicitly here for the
# guard's own docstring-adjacent coverage), and exactly "1" skips migrate
# while the server still starts.

rm -f "$MIGRATE_STARTED_AT_FILE" "$SERVER_STARTED_FILE" "$SERVER_STARTED_AT_FILE"
(
  cd "$TEST_DIR"
  unset MULTICA_SKIP_MIGRATIONS
  ./entrypoint.sh
)
if [ ! -s "$MIGRATE_STARTED_AT_FILE" ]; then
  echo "MULTICA_SKIP_MIGRATIONS unset: migrate did not run"
  exit 1
fi
if [ "$(cat "$SERVER_STARTED_FILE" 2>/dev/null || true)" != "started" ]; then
  echo "MULTICA_SKIP_MIGRATIONS unset: server did not start"
  exit 1
fi

rm -f "$MIGRATE_STARTED_AT_FILE" "$SERVER_STARTED_FILE" "$SERVER_STARTED_AT_FILE"
(
  cd "$TEST_DIR"
  export MULTICA_SKIP_MIGRATIONS=1
  ./entrypoint.sh
)
if [ -s "$MIGRATE_STARTED_AT_FILE" ]; then
  echo "MULTICA_SKIP_MIGRATIONS=1: migrate ran but should have been skipped"
  exit 1
fi
if [ "$(cat "$SERVER_STARTED_FILE" 2>/dev/null || true)" != "started" ]; then
  echo "MULTICA_SKIP_MIGRATIONS=1: server did not start"
  exit 1
fi

echo "MULTICA_SKIP_MIGRATIONS guard ok"
