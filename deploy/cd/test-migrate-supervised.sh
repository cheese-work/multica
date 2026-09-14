#!/usr/bin/env bash
set -euo pipefail

# D2 supervised migration runner tests (CHE-372). Builds the real
# server/cmd/migrate binary and runs it, supervised, against a throwaway
# isolated postgres:16-alpine container — never C00, never restored or
# production data. Requires Go on PATH (or GO_BIN set to the go binary);
# skips with a clear message if unavailable, rather than faking a result.
#
# Covers: a full successful migration run against the complete current
# migration set (positive control) with Go-level enforced timeouts proven
# to defeat a connection-string bypass attempt, a deadline-exceeded run
# that must cancel the migrator, confirm SERVER-SIDE session termination
# (not just OS process exit), and never report success, plus the
# observer-name-spoofing negative control from the quiescence gates.

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

fake_migrate_src_dir="server/cmd/che372_test_fake_migrate_DELETE_ME"
real_session_nonzero_src_dir="server/cmd/che372_test_fake_migrate_nonzero_DELETE_ME"

cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  rm -rf "$fake_migrate_src_dir"
  rm -rf "$real_session_nonzero_src_dir"
  rm -rf "$work_dir"
}
trap cleanup EXIT

echo "==> building server/cmd/migrate"
(cd server && "$go_bin" build -o "$work_dir/migrate" ./cmd/migrate)

echo "==> building the real-Postgres-session fake migrate binary (test-only, never committed)"
# This is a genuine Go binary sharing server/go.mod's dependency graph
# (needs pgx), so it is built the same way as the real migrate binary:
# temporarily placed inside the module tree, built, then removed. It is
# NEVER committed -- fake_migrate_src_dir is removed unconditionally by
# the cleanup trap above, including on failure.
mkdir -p "$fake_migrate_src_dir"
cat > "$fake_migrate_src_dir/main.go" <<'FAKE_MIGRATE_GO_EOF'
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// Test-only fake "migrate" binary for deploy/cd/test-migrate-supervised.sh's
// hard-deadline control. Unlike a shell script that spawns `sleep` as a
// child process (which signals sent to the wrapper's recorded PID cannot
// reach), this holds a REAL PostgreSQL session in a single Go process with
// no exec'd children -- matching server/cmd/migrate's actual process
// shape -- and ignores SIGTERM entirely so the wrapper is forced down its
// SIGKILL escalation path. It only exits on SIGKILL (uninterceptable) or
// the fixed upper bound below.
func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		fmt.Fprintln(os.Stderr, "fake-migrate: DATABASE_URL is required")
		os.Exit(2)
	}
	conn, err := pgx.Connect(context.Background(), dbURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake-migrate: connect failed:", err)
		os.Exit(1)
	}
	defer conn.Close(context.Background())
	fmt.Fprintln(os.Stderr, "fake-migrate: connected, ignoring SIGTERM, holding session")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for range sigCh {
			fmt.Fprintln(os.Stderr, "fake-migrate: received a signal, ignoring it")
		}
	}()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		_, _ = conn.Exec(context.Background(), "SELECT 1")
		time.Sleep(200 * time.Millisecond)
	}
}
FAKE_MIGRATE_GO_EOF
(cd server && "$go_bin" build -o "$work_dir/fake-migrate-real-session" ./cmd/che372_test_fake_migrate_DELETE_ME)
rm -rf "$fake_migrate_src_dir"

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

echo "==> unit: Go-level enforced timeouts defeat a connection-string bypass attempt"
# This is Astra's P1 finding directly: a client-supplied connection-string
# "options=" parameter is applied by pgx AFTER role/database defaults and
# therefore overrides them completely — a role/database default (this
# repo's earlier, since-removed approach) or PGOPTIONS alone is
# bypassable. The fix moved enforcement into the migrator's own Go
# process (server/internal/dbstartup.NewPoolWithEnforcedTimeouts), which
# runs a SET on every connection strictly after pgx applies the
# connection string, so it is always the LAST setting applied. Prove it
# directly against a URL that tries exactly this bypass.
bypass_url="postgres://$db_user:$db_pass@localhost:${host_port}/$db_name?sslmode=disable&options=-c%20statement_timeout%3D0%20-c%20lock_timeout%3D0"
(cd server && MULTICA_TEST_D2_BYPASS_DATABASE_URL="$bypass_url" "$go_bin" test ./internal/dbstartup/... -run TestEnforcedTimeoutsDefeatConnectionStringBypass -v) \
  && { echo "PASS: Go-level enforcement defeats the connection-string options= bypass"; pass=$((pass + 1)); } \
  || { echo "FAIL: Go-level enforcement did not defeat the connection-string bypass"; fail=$((fail + 1)); }

echo "==> positive control: full migration set completes within a generous deadline, real enforced timeouts"
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

if grep -q "confirmed live migrator Postgres session" "$work_dir/positive.log"; then
  echo "PASS: positive run confirmed the live migrator session's identity (pid+backend_start), not just an OS-level check"
  pass=$((pass + 1))
else
  echo "FAIL: positive run log is missing the live-session identity confirmation — see $work_dir/positive.log"
  fail=$((fail + 1))
fi

ledger_head="$(docker exec "$container" psql -U "$db_user" -d "$db_name" -At -c \
  "SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1;")"
if [ -n "$ledger_head" ]; then
  echo "PASS: ledger head after positive run is $ledger_head"
  pass=$((pass + 1))
else
  echo "FAIL: ledger is empty after a claimed-successful migration run"
  fail=$((fail + 1))
fi

echo "==> negative control: observer-name spoofing must not hide a foreign session from the final gate"
# A foreign session claiming the removed hardcoded observer application_name
# prefix must still be visible to quiescence.mjs's checks — this is the
# exact hole an earlier review found: a prior version excluded any row
# whose client-controlled application_name matched a fixed prefix, so a
# foreign client could claim that name and hide an open transaction from
# the final gate. There must be no such client-controlled exclusion left
# anywhere.
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

echo "==> negative control: verify-live-session-is-absent same-role session ambiguity"
# With another live session on the same role/database present, absence of
# a DIFFERENT, already-gone pid must NOT be reported as confirmed absence
# of "everything migrator-attributable" — this is Sol's second finding:
# the previous version compared against whatever an unrelated LIMIT-1
# query happened to return, so a same-role session that is not the
# recorded one could be mistaken for proof the recorded one is gone.
docker exec -d "$container" psql -U "$db_user" -d "$db_name" -c "SELECT pg_sleep(8);"
sleep 1
# This probe uses the CLI's default --query-timeout-ms (2000ms,
# DEFAULT_WALL_CLOCK_TIMEOUT_MS) against a real docker-exec'd psql round
# trip, which under CI/runner load can genuinely exceed 2s with nothing
# wrong in the code under test -- that shows up as exit 2 (observer
# failure/unknown), not the exit 1 this control expects, and is exactly
# the class of flake Astra flagged as needing a real fix, not a wider
# timeout or a "known flaky" comment. A single bounded retry ONLY on exit
# 2 distinguishes that from an actual wrong-answer regression: exit 0
# (wrongly reported absent) or any code other than 1/2 fails immediately,
# no retry, on either attempt -- only "the observer genuinely could not
# complete in time" gets a second try within the same test budget.
absent_attempt=1
absent_status=""
absent_result=""
while [ "$absent_attempt" -le 2 ]; do
  set +e
  absent_result="$(node deploy/cd/quiescence.mjs verify-live-session-is-absent --database-url "$observer_db_url" --psql-via-docker-network "$network" \
    --pid 999999 --backend-start "2001-01-01 00:00:00+00" --role-name "$db_user" --database-name "$db_name" 2>&1)"
  absent_status=$?
  set -e
  if [ "$absent_status" -eq 1 ]; then
    break
  fi
  if [ "$absent_status" -eq 2 ] && [ "$absent_attempt" -eq 1 ]; then
    echo "RETRY: verify-live-session-is-absent returned exit 2 (observer failure/unknown) on attempt 1 — retrying once before treating this as a real result: $absent_result"
    absent_attempt=$((absent_attempt + 1))
    continue
  fi
  break
done
if [ "$absent_status" -eq 1 ]; then
  echo "PASS: verify-live-session-is-absent correctly reports confirmed-still-live (exit 1) while an unrelated same-role/database session remains: $absent_result"
  pass=$((pass + 1))
else
  echo "FAIL: expected exit 1 (confirmed still live/ambiguous) while an unrelated same-role session is present, got $absent_status after $absent_attempt attempt(s) — any other status (0=wrongly absent, 2=observer failure on retry too) is the wrong failure class: $absent_result"
  fail=$((fail + 1))
fi
docker exec "$container" psql -U "$db_user" -d "$db_name" -At -c \
  "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE '%pg_sleep(8)%';" >/dev/null 2>&1 || true
sleep 1

echo "==> negative control: observer failure must never be treated as confirmed absence"
# A broken/unreachable observer connection is UNKNOWN state, not proof of
# termination -- this is Sol's first finding: the previous version
# returned success (exit 0, "absent") on ANY observer command failure.
bad_observer_url="postgres://wronguser:wrongpass@$container:5432/$db_name"
set +e
node deploy/cd/quiescence.mjs verify-live-session-is-absent --database-url "$bad_observer_url" --psql-via-docker-network "$network" \
  --pid 1 --backend-start "2001-01-01 00:00:00+00" --role-name "$db_user" --database-name "$db_name" >"$work_dir/observer-failure.log" 2>&1
observer_failure_status=$?
set -e
if [ "$observer_failure_status" -eq 2 ]; then
  echo "PASS: verify-live-session-is-absent reports observer failure as UNKNOWN (exit 2), never as confirmed absence"
  pass=$((pass + 1))
else
  echo "FAIL: expected exit 2 (observer failure/unknown) on a broken observer connection, got $observer_failure_status — see $work_dir/observer-failure.log"
  cat "$work_dir/observer-failure.log"
  fail=$((fail + 1))
fi

echo "==> negative control: a migrator backend never discovered must not become success"
# migrate-supervised.sh must never claim success when migrator_pg_pid was
# never positively identified -- Sol's finding that the previous version
# silently skipped its own fail-closed check when the Postgres session was
# never found (the `if [ -n "$migrator_pg_pid" ]` guard meant an empty
# value bypassed the check entirely, leaving a stale final_state=success).
# Force this by pointing --observer-database-url at an address the
# discovery probe can never reach, while --database-url (what the
# migrator itself uses) remains valid so the migration actually succeeds
# from the OS process's point of view.
docker exec "$container" psql -U "$db_user" -d "$db_name" -c \
  "DROP SCHEMA public CASCADE; CREATE SCHEMA public;" >/dev/null
unreachable_observer_url="postgres://$db_user:$db_pass@127.0.0.1:1/$db_name"
set +e
bash deploy/cd/migrate-supervised.sh \
  --database-url "$host_db_url" \
  --observer-database-url "$unreachable_observer_url" \
  --migrate-binary "$work_dir/migrate" \
  --role-name "$db_user" --database-name "$db_name" \
  --migration-allocation-seconds 30 \
  --psql-via-docker-network "$network" >"$work_dir/no-discovery.log" 2>&1
no_discovery_status=$?
set -e
if [ "$no_discovery_status" -eq 3 ]; then
  echo "PASS: migrate-supervised.sh exits exactly 3 (needs_operator) when the migrator's Postgres session could never be discovered/confirmed"
  pass=$((pass + 1))
else
  echo "FAIL: expected exit 3 (needs_operator) when the migrator's Postgres session could never be discovered/confirmed, got $no_discovery_status — any other status (0=wrongly success, 1/2=wrong failure class) does not match the claimed contract"
  cat "$work_dir/no-discovery.log"
  fail=$((fail + 1))
fi

echo "==> reset to blank schema for the negative case"
docker exec "$container" psql -U "$db_user" -d "$db_name" -c \
  "DROP SCHEMA public CASCADE; CREATE SCHEMA public;" >/dev/null

echo "==> negative: deadline exceeded mid-migration must cancel, confirm SERVER-SIDE absence, and never report success"
# allocation must exceed reserve with real margin (work_deadline =
# deadline - reserve must stay positive at the moment the script starts,
# or it denies entry outright before ever launching the migrator) while
# still giving a genuinely SIGTERM-responsive migrator (an ordinary Go
# process, unlike the SIGTERM-resistant fake used in the hard-deadline
# test below) enough real remaining budget after cancellation to actually
# exit and for one absence probe to clear MIN_OBSERVER_BUDGET_MS
# (calibrated to real --psql-via-docker-network overhead, ~600ms) before
# deadline_epoch. Reserve of 2s gives that probe genuine room instead of
# racing the hard-deadline enforcement to a near-certain "unconfirmed."
set +e
bash deploy/cd/migrate-supervised.sh \
  --database-url "$host_db_url" \
  --observer-database-url "$observer_db_url" \
  --migrate-binary "$work_dir/migrate" \
  --role-name "$db_user" --database-name "$db_name" \
  --migration-allocation-seconds 4 \
  --reserve-seconds 2 \
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

# The real migrate binary races an unpredictable amount of the full
# migration set before cancellation lands, so whether the confirming
# absence probe completes before the absolute deadline is a genuine race,
# not something this test should assert a fixed side of -- both
# "confirmed terminated" and "termination UNCONFIRMED" are safe,
# needs_operator, never-success outcomes. What must never appear is a
# bare signal-sent claim with no verification language at all; the
# specific deadline-vs-confirmation race is exercised deterministically
# by the hard-deadline control below instead, using a migrator that holds
# its session open indefinitely.
if grep -qE "confirmed terminated|termination UNCONFIRMED" "$work_dir/negative.log"; then
  echo "PASS: negative run recorded a verified disposition (confirmed termination or explicit UNCONFIRMED), not a bare signal-sent claim"
  pass=$((pass + 1))
else
  echo "FAIL: negative run log shows neither confirmed termination nor an explicit UNCONFIRMED report — see $work_dir/negative.log"
  cat "$work_dir/negative.log"
  fail=$((fail + 1))
fi

# A short deadline may or may not let the first migration's advisory lock
# + connection setup complete before cancellation — that race is real and
# not something this test should assert a fixed side of. What must hold
# regardless: no migrator session is left running afterward (the
# watchdog's cancellation must actually have taken effect, confirmed
# server-side via pg_stat_activity, not just via the OS-level PID).
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

echo "==> reset to blank schema before the hard-deadline control"
docker exec "$container" psql -U "$db_user" -d "$db_name" -c \
  "DROP SCHEMA public CASCADE; CREATE SCHEMA public;" >/dev/null

echo "==> negative control: the absolute deadline is a hard wall-clock boundary on cancellation itself"
# This is Sol's repeated P1: after the main cancellation loop times out,
# the SIGKILL escalation and one more absence probe must never run for
# longer than the actual remaining budget lets them. The fake migrator
# here holds a REAL PostgreSQL session (built above from
# fake_migrate_src_dir) so migrator_pg_pid is genuinely populated and the
# final absence-probe path is actually forced, not skipped because no
# session was ever discovered -- a shell script that never touches
# Postgres would leave that path untested despite a passing test.
# work_deadline (= allocation - reserve) needs enough real room for
# discovery to actually succeed against the docker-network observer's
# ~450ms overhead (MIN_OBSERVER_BUDGET_MS=600) before it matters whether
# the fake migrator is ever discovered -- unlike the earlier deadline
# test, this fake connects immediately and holds forever, so the only
# question is whether discovery has a genuine window, not whether the
# workload finishes in time. 4s allocation / 1s reserve gives work_deadline
# a healthy 3s for discovery, then reserve_seconds bounds how long
# cancellation/confirmation gets afterward -- the part this control
# actually targets.
deadline_test_allocation_seconds=4
deadline_test_reserve_seconds=1
# Sub-second, millisecond-precision tolerance -- this is the fix for
# Sol's finding that a whole-second measurement plus a multi-second
# tolerance could not distinguish the reviewed regression (which let
# confirmation work continue for roughly ONE second past the deadline)
# from correct behavior: a 1-3 second overrun would satisfy a 3-second
# tolerance either way. 900ms is chosen from actually measuring this
# exact control on this host after the fix (typical overrun 0-250ms,
# dominated by the SIGKILL-confirmation loop's own <=100ms poll interval
# plus shell/date/awk overhead) -- comfortably below the ~1000ms
# regression this control exists to catch, while still absorbing real
# process-scheduling jitter under load.
deadline_test_tolerance_ms=900

set +e
bash deploy/cd/migrate-supervised.sh \
  --database-url "$host_db_url" \
  --observer-database-url "$observer_db_url" \
  --migrate-binary "$work_dir/fake-migrate-real-session" \
  --role-name "$db_user" --database-name "$db_name" \
  --migration-allocation-seconds "$deadline_test_allocation_seconds" \
  --reserve-seconds "$deadline_test_reserve_seconds" \
  --psql-via-docker-network "$network" >"$work_dir/hard-deadline.log" 2>&1
hard_deadline_status=$?
set -e
hard_deadline_finished_ms="$(date +%s%3N)"

# Sol's finding on the previous round: expected_deadline_ms was computed by
# duplicating the supervisor's own whole-second-rounding arithmetic
# (test_started_ms + allocation*1000) from a timestamp captured on the
# TEST's clock, before the supervisor process even started. The
# supervisor independently floors ITS OWN start to whole seconds
# (migrate-supervised.sh's migration_started_epoch="$(date +%s)"), at
# whatever moment that line executes -- after bash/exec/argument-parsing
# overhead from this test's invocation. Depending on where in the current
# second each clock read landed, the real deadline_epoch could be up to
# 999ms earlier than the test's independently-duplicated estimate, which
# let the reviewed ~1000ms regression masquerade as a 1-999ms overrun and
# pass the 900ms tolerance for most start phases. Fix: read the
# supervisor's own logged work_deadline_epoch (its real, whole-second
# absolute deadline) instead of recomputing an estimate, then add back
# reserve_seconds to get its real deadline_epoch -- the actual hard-kill
# boundary this control is checking. The comparison is then between the
# supervisor's own integer-second value (converted to ms only at the
# boundary) and this test's millisecond-precision finish time, so no
# independent rounding is duplicated on the test side at all.
real_work_deadline_epoch="$(grep -o 'work_deadline=[0-9]\+' "$work_dir/hard-deadline.log" | head -1 | cut -d= -f2)"
if [ -z "$real_work_deadline_epoch" ]; then
  echo "FAIL: could not find the supervisor's logged work_deadline in $work_dir/hard-deadline.log -- cannot bind this control to the supervisor's real deadline"
  cat "$work_dir/hard-deadline.log"
  fail=$((fail + 1))
  real_deadline_epoch_ms=0
else
  real_deadline_epoch=$((real_work_deadline_epoch + deadline_test_reserve_seconds))
  real_deadline_epoch_ms=$((real_deadline_epoch * 1000))
fi

if [ "$hard_deadline_status" -eq 3 ]; then
  echo "PASS: SIGTERM-resistant migrator (real Postgres session) still resolves to exit 3 (needs_operator), never success"
  pass=$((pass + 1))
else
  echo "FAIL: expected exit 3 (needs_operator) against a SIGTERM-resistant migrator, got $hard_deadline_status — see $work_dir/hard-deadline.log"
  cat "$work_dir/hard-deadline.log"
  fail=$((fail + 1))
fi

if grep -q "confirmed live migrator Postgres session" "$work_dir/hard-deadline.log"; then
  echo "PASS: the fake migrator's real Postgres session was actually discovered, so the final absence-probe path was genuinely forced (not skipped due to an empty migrator_pg_pid)"
  pass=$((pass + 1))
else
  echo "FAIL: no live Postgres session was ever discovered for the fake migrator — the final absence-probe path was NOT exercised despite this test's intent"
  cat "$work_dir/hard-deadline.log"
  fail=$((fail + 1))
fi

if [ "$real_deadline_epoch_ms" -eq 0 ]; then
  echo "FAIL: skipping the overrun check -- no real deadline was captured from the supervisor's own log"
  fail=$((fail + 1))
else
  overrun_ms=$((hard_deadline_finished_ms - real_deadline_epoch_ms))
  if [ "$overrun_ms" -le "$deadline_test_tolerance_ms" ]; then
    echo "PASS: wrapper exited within ${overrun_ms}ms of its own real absolute deadline (work_deadline=$real_work_deadline_epoch + reserve=${deadline_test_reserve_seconds}s, tolerance ${deadline_test_tolerance_ms}ms) despite the SIGKILL escalation and final probe both being forced"
    pass=$((pass + 1))
  else
    echo "FAIL: wrapper exited ${overrun_ms}ms after its own real absolute deadline (work_deadline=$real_work_deadline_epoch + reserve=${deadline_test_reserve_seconds}s, tolerance ${deadline_test_tolerance_ms}ms) — cancellation continued past the approved envelope"
    fail=$((fail + 1))
  fi
fi

# The migrator process itself must actually be gone (SIGKILL took effect)
# even though it ignored SIGTERM. Checked by exact identity via /proc
# rather than pgrep -f, which pattern-matches full command lines and can
# false-positive against this very test script's own source text.
fake_migrate_still_running=false
for p in /proc/[0-9]*; do
  pid="${p#/proc/}"
  if [ -r "$p/exe" ] && [ "$(readlink -f "$p/exe" 2>/dev/null || true)" = "$(readlink -f "$work_dir/fake-migrate-real-session")" ]; then
    fake_migrate_still_running=true
    break
  fi
done
if $fake_migrate_still_running; then
  echo "FAIL: fake SIGTERM-resistant migrator process is still running after the wrapper exited — SIGKILL escalation did not take effect"
  fail=$((fail + 1))
else
  echo "PASS: fake SIGTERM-resistant migrator OS process is confirmed gone by exact executable identity (SIGKILL escalation took effect)"
  pass=$((pass + 1))
fi

# Parent AND child cleanup by identity: the OS process check above only
# proves the fake migrate binary's own process is gone. Separately confirm
# no Postgres session remains attributable to it either -- this is the
# server-side half of "both parent and child cleanup," verified by
# querying the real database rather than trusting the wrapper's own log
# line.
lingering_after_hard_deadline="$(docker exec "$container" psql -U "$db_user" -d "$db_name" -At -c \
  "SELECT count(*) FROM pg_stat_activity WHERE datname='$db_name' AND pid<>pg_backend_pid();" 2>/dev/null || echo "?")"
if [ "$lingering_after_hard_deadline" = "0" ]; then
  echo "PASS: no Postgres session remains after the hard-deadline SIGKILL escalation (server-side cleanup confirmed independently of the wrapper's own report)"
  pass=$((pass + 1))
else
  echo "FAIL: $lingering_after_hard_deadline Postgres session(s) still connected after the hard-deadline control claims cancellation"
  fail=$((fail + 1))
fi

echo "==> negative control: a nonzero migrator exit must be captured, not abort the script under set -e"
# This is Sol's third finding: `wait "$migrator_os_pid"` under top-level
# `set -e` would previously exit migrate-supervised.sh immediately on a
# nonzero migrator exit code, before migrate_status/failed_nonzero or any
# backend-disappearance confirmation ran at all -- so this script's own
# process would simply vanish with the migrator's raw exit code instead
# of reporting exit 1 with the expected diagnostic. Use a fake "migrate"
# binary that exits nonzero immediately (no real migration attempted, no
# real connection needed) to prove the wrapper script itself survives and
# reports correctly rather than silently propagating the child's exit code.
#
# This fake NEVER opens a Postgres session, so migrate-supervised.sh's
# own migrator_pg_pid is never discovered for this attempt -- per Astra's
# nonzero-exit fix, an UNCONFIRMED Postgres session after a nonzero exit
# is exactly as dangerous as one after a deadline/cancellation (a crashed
# client can leave server-side work in flight), so the fail-closed
# default applies and this now expects exit 3 (needs_operator), not exit
# 1. The separate real-session control below proves exit 1 is still
# reachable when a session WAS discovered and confirmed absent.
fake_migrate_binary="$work_dir/fake-migrate-nonzero"
cat > "$fake_migrate_binary" <<'FAKE_EOF'
#!/bin/sh
echo "fake migrate: simulating a nonzero exit" >&2
exit 7
FAKE_EOF
chmod +x "$fake_migrate_binary"
set +e
bash deploy/cd/migrate-supervised.sh \
  --database-url "$host_db_url" \
  --observer-database-url "$observer_db_url" \
  --migrate-binary "$fake_migrate_binary" \
  --role-name "$db_user" --database-name "$db_name" \
  --migration-allocation-seconds 30 \
  --psql-via-docker-network "$network" >"$work_dir/nonzero-exit.log" 2>&1
nonzero_exit_status=$?
set -e
if [ "$nonzero_exit_status" -eq 3 ]; then
  echo "PASS: migrate-supervised.sh exited 3 (needs_operator, fail-closed) on a nonzero migrator exit whose Postgres session was never discovered/confirmed"
  pass=$((pass + 1))
else
  echo "FAIL: expected migrate-supervised.sh to exit 3 (fail-closed, session never confirmed) on this nonzero migrator exit, got $nonzero_exit_status — see $work_dir/nonzero-exit.log"
  cat "$work_dir/nonzero-exit.log"
  fail=$((fail + 1))
fi
if grep -q "migrator exited non-zero before deadline" "$work_dir/nonzero-exit.log"; then
  echo "PASS: migrate-supervised.sh logged the expected non-zero-exit diagnostic, proving its own state machine ran (not a set -e abort)"
  pass=$((pass + 1))
else
  echo "FAIL: expected non-zero-exit diagnostic missing — see $work_dir/nonzero-exit.log"
  cat "$work_dir/nonzero-exit.log"
  fail=$((fail + 1))
fi

echo "==> negative control: a nonzero migrator exit WITH a confirmed-absent Postgres session must exit 1"
# Astra's explicit new-coverage requirement: prove the confirmation path
# actually runs (not just that it fails closed when no session was ever
# seen). This fake opens a real Postgres session first, so
# migrate-supervised.sh's discovery loop can find and record it, then
# exits nonzero itself and closes its own connection -- matching a real
# migrator that fails after connecting. Once the wrapper's own
# confirm_no_live_session_for_pid proves that exact session is gone, this
# is the one case where exit 1 (failed, not needs_operator) is correct.
mkdir -p "$real_session_nonzero_src_dir"
cat > "$real_session_nonzero_src_dir/main.go" <<'FAKE_NONZERO_GO_EOF'
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
)

// Test-only fake "migrate" binary: opens one real Postgres connection (so
// migrate-supervised.sh's discovery loop finds a real backend to track),
// holds it briefly so discovery has a window to observe it, then closes
// the connection cleanly and exits nonzero -- simulating a migrator that
// connects, fails partway through its work, and disconnects normally
// (not a crash that leaves the session dangling).
func main() {
	dbURL := os.Getenv("DATABASE_URL")
	conn, err := pgx.Connect(context.Background(), dbURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake migrate: connect failed:", err)
		os.Exit(7)
	}
	// Hold the connection open long enough for the supervisor's discovery
	// poll (200ms cadence) to observe it at least once before we exit.
	var one int
	_ = conn.QueryRow(context.Background(), "SELECT pg_sleep(1.5), 1").Scan(&one, &one)
	conn.Close(context.Background())
	fmt.Fprintln(os.Stderr, "fake migrate: simulating a post-connect nonzero exit")
	os.Exit(7)
}
FAKE_NONZERO_GO_EOF
(cd server && "$go_bin" build -o "$work_dir/fake-migrate-nonzero-real-session" ./cmd/che372_test_fake_migrate_nonzero_DELETE_ME)
rm -rf "$real_session_nonzero_src_dir"

set +e
bash deploy/cd/migrate-supervised.sh \
  --database-url "$host_db_url" \
  --observer-database-url "$observer_db_url" \
  --migrate-binary "$work_dir/fake-migrate-nonzero-real-session" \
  --role-name "$db_user" --database-name "$db_name" \
  --migration-allocation-seconds 30 \
  --psql-via-docker-network "$network" >"$work_dir/nonzero-exit-real-session.log" 2>&1
nonzero_real_session_status=$?
set -e
if [ "$nonzero_real_session_status" -eq 1 ]; then
  echo "PASS: migrate-supervised.sh exited 1 on a nonzero migrator exit whose Postgres session was discovered and confirmed absent"
  pass=$((pass + 1))
else
  echo "FAIL: expected exit 1 (confirmed terminated, failed not needs_operator) once a session was actually discovered and confirmed absent, got $nonzero_real_session_status — see $work_dir/nonzero-exit-real-session.log"
  cat "$work_dir/nonzero-exit-real-session.log"
  fail=$((fail + 1))
fi
if grep -q "is confirmed terminated — exit 1" "$work_dir/nonzero-exit-real-session.log"; then
  echo "PASS: migrate-supervised.sh logged the confirmed-terminated exit-1 diagnostic, proving the confirmation path (not the fail-closed default) actually ran"
  pass=$((pass + 1))
else
  echo "FAIL: expected confirmed-terminated exit-1 diagnostic missing — see $work_dir/nonzero-exit-real-session.log"
  cat "$work_dir/nonzero-exit-real-session.log"
  fail=$((fail + 1))
fi

echo
echo "==> $pass passed, $fail failed"
if [ "$fail" -ne 0 ]; then
  exit 1
fi
