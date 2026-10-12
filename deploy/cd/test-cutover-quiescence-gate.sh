#!/usr/bin/env bash
set -euo pipefail

# Regression test for CHE-655 defect 1 (cutover.sh's quiescence final-gate
# ran before the incumbent colour was drained, so the gate could never
# admit while a real backend was still serving) and defect 2
# (quiescence.mjs's --psql-via-docker-exec mode, added by the same fix, so
# a host with no `psql` on PATH — C00 — does not fall back to the much
# slower --psql-via-docker-network `docker run` shim).
#
# Unlike test-cutover.sh, which mocks quiescence.mjs entirely to test
# cutover.sh's own control flow, this exercises the REAL final-gate logic
# (evaluateFinalGate) against a REAL Postgres connection holding a genuine
# idle session — modeling exactly what an actively-serving backend's own
# connection pool looks like from the gate's point of view: a foreign
# backend with no open transaction and no lock, which
# evaluateFinalGate's "unfenced foreign session present" rule denies on
# unconditionally. This is the root cause CHE-609's live C00 qualification
# hit: cutover.sh ran this exact check while the incumbent colour was still
# up, so it could never admit, and the script exited before draining
# anything.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

container="che655-cutover-gate-pg-$$"
db_user="multica"
db_pass="synthetic-only"
db_name="multica"
# --psql-via-docker-exec runs `docker exec <container> psql ...` — psql
# executes INSIDE the container's own network namespace, so the target is
# the container's own loopback, never the container's Docker-network name
# (that addressing is --psql-via-docker-network's concern, not this one's).
db_url="postgres://$db_user:$db_pass@localhost:5432/$db_name"

cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> starting isolated synthetic postgres ($container)"
docker run -d --rm --name "$container" \
  -e "POSTGRES_USER=$db_user" -e "POSTGRES_PASSWORD=$db_pass" -e "POSTGRES_DB=$db_name" \
  postgres:16-alpine >/dev/null

for _ in $(seq 1 30); do
  if docker exec "$container" pg_isready -U "$db_user" -d "$db_name" >/dev/null 2>&1; then
    break
  fi
  sleep 0.5
done
docker exec "$container" pg_isready -U "$db_user" -d "$db_name" >/dev/null

final_gate() {
  node deploy/cd/quiescence.mjs final-gate --database-url "$db_url" --psql-via-docker-exec "$container" --query-timeout-ms 4000
}

# hold_idle_connection opens a REAL psql session that connects and then
# goes idle — no transaction, no query running — for $1 seconds. This is
# not a stub or a mocked control file: it is a genuine foreign backend in
# pg_stat_activity with no xact_start/backend_xid/backend_xmin, exactly the
# shape a serving backend's own held-open connection-pool member has (the
# issue's own reproduction: "the serving backend holds ~11 idle pool
# connections throughout"). Feeding psql a stdin pipe that only emits after
# `sleep` keeps the session connected and idle without ever opening a
# transaction, so this is a distinct (and per evaluateFinalGate's rules,
# equally denying) case from test-quiescence.sh's hold_txn/
# hold_idle_in_txn helpers, which both hold an open transaction.
hold_idle_connection() {
  docker exec -d "$container" bash -c \
    "{ sleep $1; } | psql -U $db_user -d $db_name"
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

# Positive control: clean database, no foreign sessions -> admits. Also
# proves --psql-via-docker-exec itself works end-to-end against a real
# Postgres (defect 2), independent of the drain-ordering scenario below.
expect_admit "clean db final gate via --psql-via-docker-exec" final_gate

# The regression: a still-serving incumbent's idle pool connection is open
# -> the final gate MUST deny (this is what cutover.sh's buggy ordering ran
# into on every single attempt — the gate correctly denying is not the bug;
# running it before the drain instead of after is). Then the connection is
# closed (modeling cutover.sh's drain/stop step) -> the SAME gate, same
# database, now admits.
hold_idle_connection 20
sleep 1
expect_deny "final gate denies while a still-serving incumbent's idle connection is open (CHE-655 defect 1)" final_gate
terminate_all_client_backends
sleep 1
expect_admit "final gate admits once the incumbent's connection is drained (post-drain)" final_gate

echo
echo "==> $pass passed, $fail failed"
[ "$fail" -eq 0 ]
