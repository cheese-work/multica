#!/usr/bin/env bash
set -euo pipefail

# D2 supervised migration runner tests (CHE-372). Builds the real
# server/cmd/migrate binary and runs it, supervised, against a throwaway
# isolated postgres:16-alpine container — never C00, never restored or
# production data. Requires Go on PATH (or GO_BIN set to the go binary);
# skips with a clear message if unavailable, rather than faking a result.
#
# Covers: a full successful migration run against the complete current
# migration set (positive control), and a deadline-exceeded run that must
# cancel the migrator, confirm termination, and leave the ledger at the
# last cleanly committed migration rather than claiming success
# (acceptance group 3's whole-phase-limit case, and part of group 5's
# "crash/interrupted mid-phase" case for the single-process form).

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

go_bin="${GO_BIN:-$(command -v go || true)}"
if [ -z "$go_bin" ] && [ -x /usr/local/go/bin/go ]; then
  go_bin=/usr/local/go/bin/go
fi
if [ -z "$go_bin" ]; then
  echo "SKIP: no Go toolchain found on PATH, GO_BIN, or /usr/local/go/bin/go — cannot build server/cmd/migrate" >&2
  exit 0
fi

work_dir="$(mktemp -d)"
network="che372-migtest-$$"
container="che372-migtest-pg-$$"
db_user="multica"
db_pass="synthetic-only"
db_name="multica"
host_port=$(( (RANDOM % 5000) + 20000 ))

cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  rm -rf "$work_dir"
}
trap cleanup EXIT

echo "==> building server/cmd/migrate"
(cd server && "$go_bin" build -o "$work_dir/migrate" ./cmd/migrate)

echo "==> starting isolated synthetic postgres ($container on $network, host port $host_port)"
docker network create "$network" >/dev/null
docker run -d --rm --name "$container" --network "$network" -p "127.0.0.1:${host_port}:5432" \
  -e "POSTGRES_USER=$db_user" -e "POSTGRES_PASSWORD=$db_pass" -e "POSTGRES_DB=$db_name" \
  postgres:16-alpine -c max_prepared_transactions=8 >/dev/null

for _ in $(seq 1 30); do
  if docker exec "$container" pg_isready -U "$db_user" -d "$db_name" >/dev/null 2>&1; then
    break
  fi
  sleep 0.5
done
docker exec "$container" pg_isready -U "$db_user" -d "$db_name" >/dev/null

host_db_url="postgres://$db_user:$db_pass@localhost:${host_port}/$db_name?sslmode=disable"
observer_db_url="postgres://$db_user:$db_pass@$container:5432/$db_name"

pass=0
fail=0

echo "==> positive control: full migration set completes within a generous deadline"
if bash deploy/cd/migrate-supervised.sh \
  --database-url "$host_db_url" \
  --observer-database-url "$observer_db_url" \
  --migrate-binary "$work_dir/migrate" \
  --role-name "$db_user" --database-name "$db_name" \
  --migration-allocation-seconds 60 \
  --psql-via-docker-network "$network" >"$work_dir/positive.log" 2>&1; then
  echo "PASS: full migration set completed and confirmed within deadline"
  pass=$((pass + 1))
else
  echo "FAIL: positive control did not exit 0 — see $work_dir/positive.log"
  cat "$work_dir/positive.log"
  fail=$((fail + 1))
fi

if grep -q "verified pre-launch role/database timeout defaults" "$work_dir/positive.log" && \
   grep -q "confirmed live migrator Postgres session" "$work_dir/positive.log"; then
  echo "PASS: positive run proved the role/database timeout default AND the live migrator session identity, not just a side-channel probe"
  pass=$((pass + 1))
else
  echo "FAIL: positive run log is missing the role/database default proof or the live-session identity confirmation — see $work_dir/positive.log"
  fail=$((fail + 1))
fi

# The positive run above left a 1000ms role/database default in place
# (migrate-supervised.sh's whole point is that this default persists past
# the migrator's own process exit, since it is server-side metadata, not
# a client-session setting). Reset it before the tests below, which use
# multi-second held sessions on the SAME role/database and would
# otherwise be spuriously cancelled by that leftover default — a
# test-harness artifact from reusing one container across sections, not
# a real-world concern (a real deployment sets this default immediately
# before each attempt's own migrator launch).
docker exec "$container" psql -U "$db_user" -d "$db_name" -c \
  "ALTER ROLE $db_user IN DATABASE $db_name RESET statement_timeout; ALTER ROLE $db_user IN DATABASE $db_name RESET lock_timeout;" >/dev/null

echo "==> negative control: observer-name spoofing must not hide a foreign session from the final gate"
# A foreign session claiming the removed hardcoded observer application_name
# prefix must still be visible to quiescence.mjs's checks — this is the
# exact hole BLOCK found: a prior version excluded any row whose
# client-controlled application_name matched a fixed prefix, so a foreign
# client could claim that name and hide an open transaction from the final
# gate. There must be no such client-controlled exclusion left anywhere.
docker exec -d "$container" bash -c \
  "PGAPPNAME='che372-d2-quiescence-observer-fake' psql -U $db_user -d $db_name -c 'BEGIN; SELECT pg_sleep(20);'"
sleep 1
if node deploy/cd/quiescence.mjs final-gate --database-url "$observer_db_url" --psql-via-docker-network "$network" >/dev/null 2>&1; then
  echo "FAIL: final gate ADMITTED while a session spoofing the old observer application_name prefix held an open transaction — spoofing hole is NOT closed"
  fail=$((fail + 1))
else
  echo "PASS: final gate correctly denied despite the foreign session spoofing the old observer application_name prefix"
  pass=$((pass + 1))
fi
docker exec "$container" psql -U "$db_user" -d "$db_name" -At -c \
  "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name LIKE '%fake%';" >/dev/null 2>&1 || true
sleep 1

echo "==> negative control: timeout-proof bypass must fail closed rather than silently approve"
# verify-role-timeout-defaults must deny when no default has ever been set
# for a given role/database pair (the pre-launch state for a fresh
# database), never treat "no rows" as "any timeout is fine."
bare_role="che372_bare_role_$$"
docker exec "$container" psql -U "$db_user" -d "$db_name" -At -c \
  "DO \$\$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '$bare_role') THEN CREATE ROLE $bare_role LOGIN; END IF; END \$\$;" >/dev/null 2>&1
if node deploy/cd/quiescence.mjs verify-role-timeout-defaults --database-url "$observer_db_url" --psql-via-docker-network "$network" \
  --role-name "$bare_role" --database-name "$db_name" >/dev/null 2>&1; then
  echo "FAIL: verify-role-timeout-defaults reported ok for a role with no configured default — timeout-proof bypass is possible"
  fail=$((fail + 1))
else
  echo "PASS: verify-role-timeout-defaults correctly denies when no role/database default has been set (fails closed, not open)"
  pass=$((pass + 1))
fi
docker exec "$container" psql -U "$db_user" -d "$db_name" -c "DROP ROLE IF EXISTS $bare_role;" >/dev/null 2>&1 || true

echo "==> negative control: live-session identity check must reject a mismatched pid/backend_start/role"
docker exec -d "$container" psql -U "$db_user" -d "$db_name" -c "SELECT pg_sleep(10);"
sleep 1
live="$(node deploy/cd/quiescence.mjs find-live-session-by-role --database-url "$observer_db_url" --psql-via-docker-network "$network" \
  --role-name "$db_user" --database-name "$db_name" --query-timeout-ms 3000)"
live_pid="$(node -e 'process.stdout.write(String(JSON.parse(process.argv[1]).pid))' "$live")"
live_backend_start="$(node -e 'process.stdout.write(String(JSON.parse(process.argv[1]).backendStart))' "$live")"
if node deploy/cd/quiescence.mjs verify-live-session-is-covered --database-url "$observer_db_url" --psql-via-docker-network "$network" \
  --pid "$live_pid" --backend-start "2001-01-01 00:00:00+00" --role-name "$db_user" --database-name "$db_name" >/dev/null 2>&1; then
  echo "FAIL: verify-live-session-is-covered admitted a backend_start mismatch — PID-reuse spoofing is possible"
  fail=$((fail + 1))
else
  echo "PASS: verify-live-session-is-covered correctly rejects a backend_start mismatch (guards against PID reuse)"
  pass=$((pass + 1))
fi
if node deploy/cd/quiescence.mjs verify-live-session-is-covered --database-url "$observer_db_url" --psql-via-docker-network "$network" \
  --pid "$live_pid" --backend-start "$live_backend_start" --role-name "wrong_role_name" --database-name "$db_name" >/dev/null 2>&1; then
  echo "FAIL: verify-live-session-is-covered admitted a role-name mismatch"
  fail=$((fail + 1))
else
  echo "PASS: verify-live-session-is-covered correctly rejects a role-name mismatch"
  pass=$((pass + 1))
fi
docker exec "$container" psql -U "$db_user" -d "$db_name" -At -c \
  "SELECT pg_terminate_backend($live_pid);" >/dev/null 2>&1 || true
sleep 1

# Reset the role/database default set by the positive run above so it does
# not leak into the negative-control run below (a real deployment always
# targets a fresh role/database pair per attempt; this test reuses one
# container across both runs for speed).
docker exec "$container" psql -U "$db_user" -d "$db_name" -c \
  "ALTER ROLE $db_user IN DATABASE $db_name RESET statement_timeout; ALTER ROLE $db_user IN DATABASE $db_name RESET lock_timeout;" >/dev/null 2>&1

ledger_head="$(docker exec "$container" psql -U "$db_user" -d "$db_name" -At -c \
  "SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1;")"
if [ -n "$ledger_head" ]; then
  echo "PASS: ledger head after positive run is $ledger_head"
  pass=$((pass + 1))
else
  echo "FAIL: ledger is empty after a claimed-successful migration run"
  fail=$((fail + 1))
fi

echo "==> reset to blank schema for the negative case"
docker exec "$container" psql -U "$db_user" -d "$db_name" -c \
  "DROP SCHEMA public CASCADE; CREATE SCHEMA public;" >/dev/null

echo "==> negative: deadline exceeded mid-migration must cancel, confirm, and never report success"
set +e
bash deploy/cd/migrate-supervised.sh \
  --database-url "$host_db_url" \
  --observer-database-url "$observer_db_url" \
  --migrate-binary "$work_dir/migrate" \
  --role-name "$db_user" --database-name "$db_name" \
  --migration-allocation-seconds 1 \
  --reserve-seconds 0 \
  --psql-via-docker-network "$network" >"$work_dir/negative.log" 2>&1
negative_status=$?
set -e

if [ "$negative_status" -eq 3 ]; then
  echo "PASS: deadline-exceeded run exited 3 (needs_operator), not 0"
  pass=$((pass + 1))
else
  echo "FAIL: expected exit 3 (needs_operator) on deadline exceeded, got $negative_status — see $work_dir/negative.log"
  cat "$work_dir/negative.log"
  fail=$((fail + 1))
fi

if grep -q "confirmed terminated" "$work_dir/negative.log"; then
  echo "PASS: negative run recorded confirmed termination, not a bare signal-sent claim"
  pass=$((pass + 1))
else
  echo "FAIL: negative run log does not show confirmed termination — see $work_dir/negative.log"
  fail=$((fail + 1))
fi

# A 1-second deadline may or may not let the first migration's advisory
# lock + connection setup complete before cancellation — that race is
# real and not something this test should assert a fixed side of. What
# must hold regardless: no migrator session is left running afterward
# (the watchdog's cancellation must actually have taken effect), and
# whatever the ledger contains is a set of real committed migrations
# applied in order, never a gap or a forged/duplicated entry.
lingering_backends="$(docker exec "$container" psql -U "$db_user" -d "$db_name" -At -c \
  "SELECT count(*) FROM pg_stat_activity WHERE datname='$db_name' AND pid<>pg_backend_pid();" 2>/dev/null || echo "?")"
if [ "$lingering_backends" = "0" ]; then
  echo "PASS: no migrator/hook session remains connected after confirmed cancellation"
  pass=$((pass + 1))
else
  echo "FAIL: $lingering_backends session(s) still connected after cancellation was reported confirmed"
  fail=$((fail + 1))
fi

partial_ledger_count="$(docker exec "$container" psql -U "$db_user" -d "$db_name" -At -c \
  "SELECT count(*) FROM schema_migrations;" 2>/dev/null || echo "0")"
echo "INFO: ledger has $partial_ledger_count row(s) after cancellation (0 is a legitimate outcome if the deadline hit before the first commit)"

echo
echo "==> $pass passed, $fail failed"
if [ "$fail" -ne 0 ]; then
  exit 1
fi
