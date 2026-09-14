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

preflight() {
  node deploy/cd/quiescence.mjs preflight --database-url "$db_url" --psql-via-docker-network "$network"
}
final_gate() {
  node deploy/cd/quiescence.mjs final-gate --database-url "$db_url" --psql-via-docker-network "$network"
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

# Short metadata-only observer sessions (i.e. this tool's own connections)
# must not block the positive control — already implicit in every
# "expect_admit" case above, since each call is itself a fresh psql
# connection with application_name che372-d2-quiescence-observer, and the
# module must exclude its own concurrent/adjacent calls.
expect_admit "controller's own observation connections never self-block" preflight
expect_admit "controller's own observation connections never self-block (final gate)" final_gate

echo "==> verify-timeouts"
result="$(node deploy/cd/quiescence.mjs verify-timeouts --database-url "$db_url" --psql-via-docker-network "$network")"
echo "$result" | grep -q '"ok":true' && { echo "PASS: verify-timeouts reads back positive session timeouts"; pass=$((pass + 1)); } \
  || { echo "FAIL: verify-timeouts :: $result"; fail=$((fail + 1)); }

echo
echo "==> $pass passed, $fail failed"
if [ "$fail" -ne 0 ]; then
  exit 1
fi
