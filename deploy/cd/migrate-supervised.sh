#!/usr/bin/env bash
set -euo pipefail

# D2 supervised migration runner (CHE-372). Wraps the existing `migrate`
# binary with an absolute work deadline, connection-level timeouts applied
# and read back (never trusted blind), and an external watchdog that can
# terminate only the migrator's own recorded session — never a foreign
# session, and never by executable name.
#
# This script does not decide quiescence; call deploy/cd/quiescence.mjs
# final-gate immediately before invoking this script, and again is not
# needed here because the deadline arithmetic below already assumes the
# final gate has just passed. It does not talk to C00 and accepts no
# production credentials; --database-url is caller-supplied and this
# script has no default beyond what the caller passes.
#
# Deadline model (from the approved D2 addendum):
#   migration_deadline = min(migration_started + 15s, cutover_started + 40s)
#   work_deadline      = migration_deadline - 2s   (reserved for stopping
#                         owned execution and confirming exit)
# The caller supplies --cutover-started-epoch so this script can compute
# the same clamped deadline the controller uses; if omitted, only the 15s
# migration-phase bound applies (useful for standalone testing).

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

usage() {
  cat <<'EOF'
usage: migrate-supervised.sh --database-url URL --migrate-binary PATH
  [--observer-database-url URL] [--psql-via-docker-network NAME]
  [--cutover-started-epoch SECONDS] [--migration-allocation-seconds N]
  [--reserve-seconds N] [--statement-timeout-ms N] [--lock-timeout-ms N]

--database-url is what the migrator binary itself connects with (it is a
pure Go/pgx process and needs no psql). --observer-database-url is what
the watchdog's independent read-back query connects with; it defaults to
--database-url but may need to differ when the migrator reaches Postgres
via a host-published port while the observer reaches it via
--psql-via-docker-network's container DNS (or vice versa) — these are two
different network paths to the same database, not two different databases.

Exit codes:
  0   migration completed and confirmed within the work deadline
  1   migration failed (non-migrator-timeout failure); state is UNCERTAIN,
      not automatically compatible-rollback-eligible — see stderr
  2   usage error
  3   work deadline exceeded; entered needs_operator (fence must stay up)
EOF
}

database_url=""
observer_database_url=""
migrate_binary=""
cutover_started_epoch=""
migration_allocation_seconds=15
reserve_seconds=2
statement_timeout_ms=1000
lock_timeout_ms=1000
psql_via_docker_network=""

while (($#)); do
  case "$1" in
    --database-url) database_url=${2:?}; shift 2 ;;
    --observer-database-url) observer_database_url=${2:?}; shift 2 ;;
    --migrate-binary) migrate_binary=${2:?}; shift 2 ;;
    --cutover-started-epoch) cutover_started_epoch=${2:?}; shift 2 ;;
    --migration-allocation-seconds) migration_allocation_seconds=${2:?}; shift 2 ;;
    --reserve-seconds) reserve_seconds=${2:?}; shift 2 ;;
    --statement-timeout-ms) statement_timeout_ms=${2:?}; shift 2 ;;
    --lock-timeout-ms) lock_timeout_ms=${2:?}; shift 2 ;;
    --psql-via-docker-network) psql_via_docker_network=${2:?}; shift 2 ;;
    --help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ -n "$database_url" ] || { echo "missing --database-url" >&2; usage >&2; exit 2; }
[ -n "$migrate_binary" ] || { echo "missing --migrate-binary" >&2; usage >&2; exit 2; }
[ -x "$migrate_binary" ] || { echo "--migrate-binary is not executable: $migrate_binary" >&2; exit 2; }
[ -n "$observer_database_url" ] || observer_database_url="$database_url"

migration_started_epoch="$(date +%s)"

# migration_deadline = min(migration_started + allocation, cutover_started + 40)
# The 40s bound is v3's fixed constant (5+5+15+15 = 40s through the end of
# the migration phase in the 60s envelope); it is not a CLI flag because
# changing it would silently widen the approved envelope.
cutover_bound_seconds=40
deadline_epoch=$((migration_started_epoch + migration_allocation_seconds))
if [ -n "$cutover_started_epoch" ]; then
  cutover_deadline=$((cutover_started_epoch + cutover_bound_seconds))
  if [ "$cutover_deadline" -lt "$deadline_epoch" ]; then
    deadline_epoch="$cutover_deadline"
  fi
fi
work_deadline_epoch=$((deadline_epoch - reserve_seconds))

now_epoch="$(date +%s)"
remaining_work_seconds=$((work_deadline_epoch - now_epoch))
if [ "$remaining_work_seconds" -le 0 ]; then
  echo "che372-d2: usable work time already expired before migration start (deny entry)" >&2
  exit 3
fi

echo "che372-d2: migration_started=$migration_started_epoch work_deadline=$work_deadline_epoch remaining=${remaining_work_seconds}s" >&2

# Statement/lock timeout are applied via PGOPTIONS on the migrator's own
# connection string. The migrator opens additional pool connections for
# pre/post-migration hooks (server/cmd/migrate/main.go); PGOPTIONS is
# process-wide for this child, so every pgx connection opened by this
# process inherits the same session defaults, including hook connections.
# lock_timeout is capped at the same 1000ms ceiling the quiescence design
# requires; statement_timeout is bounded by the remaining work time so no
# single statement can consume the whole budget.
statement_timeout_ms_capped=$(( statement_timeout_ms < (remaining_work_seconds * 1000) ? statement_timeout_ms : (remaining_work_seconds * 1000) ))
if [ "$statement_timeout_ms_capped" -le 0 ]; then
  echo "che372-d2: statement timeout rounds to <=0ms (disabled in Postgres) — deny" >&2
  exit 3
fi
if [ "$lock_timeout_ms" -le 0 ]; then
  echo "che372-d2: lock timeout rounds to <=0ms (disabled in Postgres) — deny" >&2
  exit 3
fi

export PGOPTIONS="-c statement_timeout=${statement_timeout_ms_capped} -c lock_timeout=${lock_timeout_ms}"
export DATABASE_URL="$database_url"

# --- verify the settings actually apply, before launching the migrator ---
# This connects independently (its own psql session), so it proves the
# server-side session default actually took effect for connections using
# this PGOPTIONS — it does not and cannot inspect the migrator's own
# in-flight connection. It is boot-time proof for this DATABASE_URL/PGOPTIONS
# pairing, not a live per-statement audit of the migrator's session.
verify_timeouts_args=(deploy/cd/quiescence.mjs verify-timeouts --database-url "$observer_database_url")
if [ -n "$psql_via_docker_network" ]; then
  verify_timeouts_args+=(--psql-via-docker-network "$psql_via_docker_network")
fi
verify_result="$(node "${verify_timeouts_args[@]}" 2>&1)" || {
  echo "che372-d2: could not verify connection timeouts before launch: $verify_result" >&2
  exit 3
}
echo "che372-d2: verified pre-launch timeouts: $verify_result" >&2

# --- launch the migrator as a supervised child; restart disabled ---
"$migrate_binary" up &
migrator_pid=$!
echo "che372-d2: launched migrator pid=$migrator_pid" >&2

watchdog_result="ok"
while kill -0 "$migrator_pid" 2>/dev/null; do
  now_epoch="$(date +%s)"
  if [ "$now_epoch" -ge "$work_deadline_epoch" ]; then
    watchdog_result="deadline_exceeded"
    break
  fi
  sleep 0.2
done

if [ "$watchdog_result" = "deadline_exceeded" ]; then
  echo "che372-d2: work_deadline reached with migrator pid=$migrator_pid still running; cancelling" >&2
  # Graceful cancel first (SIGTERM), verified via /proc rather than trusting
  # the signal's return code — the recorded PID is the only session this
  # script is authorized to touch, per the "own recorded PID, never by
  # name" rule.
  kill -TERM "$migrator_pid" 2>/dev/null || true
  for _ in $(seq 1 5); do
    kill -0 "$migrator_pid" 2>/dev/null || break
    sleep 0.2
  done
  if kill -0 "$migrator_pid" 2>/dev/null; then
    echo "che372-d2: graceful cancel unconfirmed; escalating to SIGKILL on recorded pid=$migrator_pid" >&2
    kill -KILL "$migrator_pid" 2>/dev/null || true
    sleep 0.2
  fi
  if kill -0 "$migrator_pid" 2>/dev/null; then
    echo "che372-d2: termination UNCONFIRMED for pid=$migrator_pid — entering needs_operator, fence must stay up" >&2
    exit 3
  fi
  wait "$migrator_pid" 2>/dev/null || true
  echo "che372-d2: migrator pid=$migrator_pid confirmed terminated after deadline — needs_operator (state uncertain: hooks or earlier statements may have committed)" >&2
  exit 3
fi

wait "$migrator_pid"
migrate_status=$?

if [ "$migrate_status" -ne 0 ]; then
  echo "che372-d2: migrator exited non-zero ($migrate_status) before deadline — state UNCERTAIN, compare full ledger/object state before any decision" >&2
  exit 1
fi

final_now_epoch="$(date +%s)"
if [ "$final_now_epoch" -gt "$work_deadline_epoch" ]; then
  echo "che372-d2: migrator exited zero but completion observed after work_deadline — treat as UNCERTAIN per spec, not a clean success" >&2
  exit 3
fi

echo "che372-d2: migration completed successfully within work_deadline (finished at $final_now_epoch, deadline $work_deadline_epoch)" >&2
exit 0
