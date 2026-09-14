#!/usr/bin/env bash
set -euo pipefail

# D2 quiescence controller tests (CHE-372). Runs against a throwaway,
# isolated postgres:16-alpine container on a dedicated Docker network —
# never C00, never any restored or production fixture. Covers acceptance
# groups 1 (preflight/drain) and 2 (snapshot/visibility) from the approved
# design; groups 3-5 (whole-phase race limits, interrupted DDL, crash
# recovery) require the supervised migration runner and are exercised by
# test-migrate-supervised.sh instead.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

network="che372-d2-test-$$"
container="che372-d2-pg-$$"
db_user="multica"
db_pass="synthetic-only"
db_name="multica"
db_url="postgres://$db_user:$db_pass@$container:5432/$db_name"

cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> starting isolated synthetic postgres ($container on $network)"
docker network create "$network" >/dev/null
docker run -d --rm --name "$container" --network "$network" \
  -e "POSTGRES_USER=$db_user" -e "POSTGRES_PASSWORD=$db_pass" -e "POSTGRES_DB=$db_name" \
  postgres:16-alpine \
  -c max_prepared_transactions=8 >/dev/null

for _ in $(seq 1 30); do
  if docker exec "$container" pg_isready -U "$db_user" -d "$db_name" >/dev/null 2>&1; then
    break
  fi
  sleep 0.5
done
docker exec "$container" pg_isready -U "$db_user" -d "$db_name" >/dev/null

# --query-timeout-ms 4000 is generous on purpose: this test suite drives
# many docker-networked observation calls in close succession, sometimes
# against sessions this same suite is holding open concurrently, and
# measured real Docker daemon overhead on a busy shared host exceeded the
# module's own 2000ms default often enough to make this suite flaky. This
# is a test-only convenience value; the deadline-safety-critical caller
# (migrate-supervised.sh) always computes and passes its own precise
# remaining-time budget and never depends on any default here.
preflight() {
  node deploy/cd/quiescence.mjs preflight --database-url "$db_url" --psql-via-docker-network "$network" --query-timeout-ms 4000
}
final_gate() {
  node deploy/cd/quiescence.mjs final-gate --database-url "$db_url" --psql-via-docker-network "$network" --query-timeout-ms 4000
}

hold_txn() {
  # Launch a background session holding an open transaction for $1 seconds
  # (BEGIN, then sleep, in the SAME statement batch so the session's
  # transaction stays open across the sleep instead of auto-committing).
  # docker exec -d detaches immediately; the container-side process keeps
  # running independently of this script.
  docker exec -d "$container" psql -U "$db_user" -d "$db_name" -c "BEGIN; SELECT pg_sleep($1);"
}

hold_idle_in_txn() {
  # Launch a background session that opens a transaction, runs one
  # statement, then truly goes idle (no query executing at all, state
  # "idle in transaction") for $1 seconds — distinct from hold_txn, whose
  # session is "active" the whole time inside pg_sleep(). Feeding psql via
  # stdin with a shell-side sleep between statements is what produces a
  # genuine idle gap; a single -c "BEGIN; SELECT pg_sleep(n)" never does.
  docker exec -d "$container" bash -c \
    "{ echo 'BEGIN;'; echo 'SELECT 1;'; sleep $1; echo 'COMMIT;'; } | psql -U $db_user -d $db_name"
}

terminate_all_client_backends() {
  docker exec "$container" psql -U "$db_user" -d "$db_name" -At -c \
    "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='$db_name' AND pid<>pg_backend_pid();" >/dev/null 2>&1 || true
}

pass=0
fail=0

expect_admit() {
  local name="$1"; shift
  local out
  if out="$("$@")"; then
    echo "PASS: $name (admitted) :: $out"
    pass=$((pass + 1))
  else
    echo "FAIL: $name expected ADMIT, got DENY :: $out"
    fail=$((fail + 1))
  fi
}

expect_deny() {
  local name="$1"; shift
  local out
  if out="$("$@")"; then
    echo "FAIL: $name expected DENY, got ADMIT :: $out"
    fail=$((fail + 1))
  else
    echo "PASS: $name (denied) :: $out"
    pass=$((pass + 1))
  fi
}

echo "==> group 1: preflight and drain"

# Positive control: clean database, nothing open.
expect_admit "clean db preflight" preflight
expect_admit "clean db final gate" final_gate

# A foreign transaction already >=15s old denies preflight before any
# service restriction is attempted (no outage started to wait it out).
hold_txn 30
sleep 16
expect_deny "old (>=15s) transaction denies preflight" preflight
terminate_all_client_backends
sleep 1
expect_admit "preflight admits again once the old transaction is gone" preflight

# A young transaction (just opened) is allowed through preflight (only
# denies final gate, and only if it is still open there).
hold_txn 30
expect_admit "young transaction allowed into preflight" preflight
expect_deny "young transaction still open denies final gate" final_gate
terminate_all_client_backends
sleep 1
expect_admit "final gate admits once the transaction drains" final_gate

echo "==> group 2: snapshot and visibility coverage"

# Idle-in-transaction: BEGIN with no active statement still holds a
# snapshot and must deny the final gate.
hold_idle_in_txn 20
sleep 2
expect_deny "idle-in-transaction session denies final gate" final_gate
terminate_all_client_backends
sleep 1
expect_admit "final gate admits once the idle-in-transaction session is gone" final_gate

# Prepared transaction: no ordinary client PID once prepared, but pg_prepared_xacts
# must still be inspected and must deny.
docker exec "$container" psql -U "$db_user" -d "$db_name" -c \
  "BEGIN; CREATE TABLE IF NOT EXISTS che372_prepared_test(id int); PREPARE TRANSACTION 'che372_d2_test_prepared';" >/dev/null
expect_deny "prepared transaction denies preflight" preflight
expect_deny "prepared transaction denies final gate" final_gate
docker exec "$container" psql -U "$db_user" -d "$db_name" -c "ROLLBACK PREPARED 'che372_d2_test_prepared';" >/dev/null
sleep 1
expect_admit "final gate admits once the prepared transaction is resolved" final_gate

# Competing advisory-lock holder.
docker exec -d "$container" bash -c \
  "psql -U $db_user -d $db_name -c 'SELECT pg_advisory_lock(4246); SELECT pg_sleep(20);' & sleep 22"
sleep 1
expect_deny "foreign advisory-lock holder denies preflight" preflight
expect_deny "foreign advisory-lock holder denies final gate" final_gate
terminate_all_client_backends
sleep 1
expect_admit "final gate admits once the advisory lock is released" final_gate

# Short metadata-only observation queries never overlap in time (runPsql
# uses spawnSync, which blocks until its single psql process exits before
# the next observation query runs), so this observer never needs to
# exclude a "second observer connection" — self-exclusion is handled
# entirely server-side via pg_backend_pid(), which a client cannot spoof.
expect_admit "controller's own observation connections never self-block" preflight
expect_admit "controller's own observation connections never self-block (final gate)" final_gate

echo "==> negative control: application_name spoofing must not hide a foreign session"
# A previous version of quiescence.mjs excluded any row whose
# client-controlled application_name matched a fixed observer prefix —
# a real admission hole, since application_name is a session GUC any
# client can set to an arbitrary string. A foreign session claiming that
# exact former prefix must still be caught by the final gate.
docker exec -d "$container" bash -c \
  "PGAPPNAME='che372-d2-quiescence-observer-fake' psql -U $db_user -d $db_name -c 'BEGIN; SELECT pg_sleep(20);'"
sleep 1
expect_deny "foreign session spoofing the old observer application_name prefix still denies final gate" final_gate
terminate_all_client_backends
sleep 1
expect_admit "final gate admits again once the spoofing session is gone" final_gate

# A role/database-scoped timeout default (ALTER ROLE ... IN DATABASE ...
# SET) was tried here previously and removed: a client-supplied
# connection-string "options=" parameter is applied by pgx AFTER
# role/database defaults and therefore overrides them completely (see
# server/internal/dbstartup.NewPoolWithEnforcedTimeouts's doc comment).
# Timeout enforcement now lives inside the migrator's own Go process, so
# there is nothing to set/verify from this shell-level observer module —
# see deploy/cd/test-migrate-supervised.sh's
# TestEnforcedTimeoutsDefeatConnectionStringBypass for that coverage.

echo "==> live session identity coverage"
docker exec -d "$container" psql -U "$db_user" -d "$db_name" -c "SELECT pg_sleep(10);"
sleep 1
live="$(node deploy/cd/quiescence.mjs find-live-session-by-role --database-url "$db_url" --psql-via-docker-network "$network" \
  --role-name "$db_user" --database-name "$db_name" --query-timeout-ms 3000)"
echo "$live" | grep -q '"ok":true' && { echo "PASS: find-live-session-by-role locates the live session"; pass=$((pass + 1)); } \
  || { echo "FAIL: find-live-session-by-role :: $live"; fail=$((fail + 1)); }
live_pid="$(node -e 'process.stdout.write(String(JSON.parse(process.argv[1]).pid))' "$live")"
live_backend_start="$(node -e 'process.stdout.write(String(JSON.parse(process.argv[1]).backendStart))' "$live")"

if node deploy/cd/quiescence.mjs verify-live-session-is-covered --database-url "$db_url" --psql-via-docker-network "$network" \
  --pid "$live_pid" --backend-start "$live_backend_start" --role-name "$db_user" --database-name "$db_name" --query-timeout-ms 4000 >/dev/null 2>&1; then
  echo "PASS: verify-live-session-is-covered confirms the correct live identity"
  pass=$((pass + 1))
else
  echo "FAIL: verify-live-session-is-covered rejected a genuinely correct identity"
  fail=$((fail + 1))
fi

echo "==> negative control: live-session identity check rejects a mismatched backend_start (PID reuse guard)"
if node deploy/cd/quiescence.mjs verify-live-session-is-covered --database-url "$db_url" --psql-via-docker-network "$network" \
  --pid "$live_pid" --backend-start "2001-01-01 00:00:00+00" --role-name "$db_user" --database-name "$db_name" >/dev/null 2>&1; then
  echo "FAIL: verify-live-session-is-covered admitted a backend_start mismatch — PID-reuse spoofing possible"
  fail=$((fail + 1))
else
  echo "PASS: verify-live-session-is-covered rejects a backend_start mismatch"
  pass=$((pass + 1))
fi
terminate_all_client_backends

echo
echo "==> $pass passed, $fail failed"
if [ "$fail" -ne 0 ]; then
  exit 1
fi
