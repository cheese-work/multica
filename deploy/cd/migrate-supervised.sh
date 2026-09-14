#!/usr/bin/env bash
set -euo pipefail

# D2 supervised migration runner (CHE-372). Wraps the existing `migrate`
# binary with an absolute work deadline that governs every phase from
# entry to exit (not just the child's own runtime), timeouts enforced and
# read back inside the migrator's own Go connection lifecycle (never a
# disconnected side-channel probe, and never defeatable by a
# connection-string "options=" parameter), and an external watchdog that
# confirms server-side session termination before claiming a cancellation
# succeeded.
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
#
# The ENTIRE body below this point — settings enforcement, launch, live-
# session discovery, the run itself, and any cancellation/confirmation —
# runs under one absolute deadline loop. There is no separate "setup"
# phase exempt from the budget, and no unbounded sleep/wait anywhere.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

usage() {
  cat <<'EOF'
usage: migrate-supervised.sh --database-url URL --migrate-binary PATH
  --role-name ROLE --database-name DBNAME
  [--observer-database-url URL] [--psql-via-docker-network NAME]
  [--cutover-started-epoch SECONDS] [--migration-allocation-seconds N]
  [--reserve-seconds N] [--statement-timeout-ms N] [--lock-timeout-ms N]

--database-url is what the migrator binary itself connects with (it is a
pure Go/pgx process and needs no psql). --observer-database-url is what
this script's own watchdog queries connect with; it defaults to
--database-url but may need to differ when the migrator reaches Postgres
via a host-published port while the observer reaches it via
--psql-via-docker-network's container DNS (or vice versa) — these are two
different network paths to the same database, not two different databases.

--role-name/--database-name identify the exact role and database the
migrator authenticates as, used only to locate its live Postgres
session(s) for watchdog termination confirmation — NOT to enforce
timeouts (see below).

Timeouts are enforced inside the migrator's own process via
MULTICA_INTERNAL_D2_ENFORCED_STATEMENT_TIMEOUT_MS/
MULTICA_INTERNAL_D2_ENFORCED_LOCK_TIMEOUT_MS (see
server/internal/dbstartup.NewPoolWithEnforcedTimeouts), which sets and
reads back statement_timeout/lock_timeout on every physical connection
the migrator's pool opens — including hook connections — strictly AFTER
pgx applies the connection string, so it cannot be defeated by a
connection-string "options=" parameter the way a role/database default or
client-side PGOPTIONS both can be (pgx applies connection-string settings
above both; verified against pgx v5's own precedence). If the migrator
process does not enforce internally (e.g. an unpatched binary), this
script has no way to guarantee the timeout — the enforcement lives inside
the migrator, and this script's job is to require it, not to substitute
for it from outside.

Exit codes:
  0   migration completed and confirmed within the work deadline
  1   migration failed (non-migrator-timeout failure); state is UNCERTAIN,
      not automatically compatible-rollback-eligible — see stderr
  2   usage error
  3   work deadline exceeded, setup could not complete in time, or
      termination could not be confirmed server-side; entered
      needs_operator (fence must stay up)
EOF
}

database_url=""
observer_database_url=""
migrate_binary=""
role_name=""
database_name=""
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
    --role-name) role_name=${2:?}; shift 2 ;;
    --database-name) database_name=${2:?}; shift 2 ;;
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
[ -n "$role_name" ] || { echo "missing --role-name" >&2; usage >&2; exit 2; }
[ -n "$database_name" ] || { echo "missing --database-name" >&2; usage >&2; exit 2; }
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

remaining_seconds() {
  local now
  now="$(date +%s)"
  echo $((work_deadline_epoch - now))
}

if [ "$(remaining_seconds)" -le 0 ]; then
  echo "che372-d2: usable work time already expired before migration start (deny entry)" >&2
  exit 3
fi

echo "che372-d2: migration_started=$migration_started_epoch work_deadline=$work_deadline_epoch remaining=$(remaining_seconds)s" >&2

statement_timeout_ms_capped=$(( statement_timeout_ms < ($(remaining_seconds) * 1000) ? statement_timeout_ms : ($(remaining_seconds) * 1000) ))
if [ "$statement_timeout_ms_capped" -le 0 ]; then
  echo "che372-d2: statement timeout rounds to <=0ms (disabled in Postgres) — deny" >&2
  exit 3
fi
if [ "$lock_timeout_ms" -le 0 ]; then
  echo "che372-d2: lock timeout rounds to <=0ms (disabled in Postgres) — deny" >&2
  exit 3
fi

export DATABASE_URL="$database_url"
export MULTICA_INTERNAL_D2_ENFORCED_STATEMENT_TIMEOUT_MS="$statement_timeout_ms_capped"
export MULTICA_INTERNAL_D2_ENFORCED_LOCK_TIMEOUT_MS="$lock_timeout_ms"

quiescence_args_common=(--database-url "$observer_database_url")
if [ -n "$psql_via_docker_network" ]; then
  quiescence_args_common+=(--psql-via-docker-network "$psql_via_docker_network")
fi

# remaining_ms_before returns how many whole milliseconds remain before
# the given absolute epoch-seconds deadline, floored at 0 — never
# negative, so callers can compare directly against a minimum floor
# without a separate sign check.
remaining_ms_before() {
  local deadline="$1"
  local now_ms
  now_ms="$(($(date +%s%3N)))"
  local deadline_ms=$((deadline * 1000))
  local remaining=$((deadline_ms - now_ms))
  if [ "$remaining" -lt 0 ]; then remaining=0; fi
  echo "$remaining"
}

# sleep_clamped_to sleeps for at most requested_ms, but never past the
# given absolute deadline — a fixed poll-interval sleep (e.g. `sleep 0.2`
# at the bottom of a watchdog loop) that ignores how close the deadline
# already is can itself carry the loop past that deadline before it is
# ever re-checked. If no time remains at all, this returns immediately
# rather than sleeping zero seconds through a shell arithmetic edge case.
sleep_clamped_to() {
  local requested_ms="$1"
  local deadline_arg="$2"
  local remaining_ms
  remaining_ms="$(remaining_ms_before "$deadline_arg")"
  if [ "$remaining_ms" -le 0 ]; then
    return 0
  fi
  local sleep_ms=$(( remaining_ms < requested_ms ? remaining_ms : requested_ms ))
  local sleep_seconds
  sleep_seconds="$(awk -v ms="$sleep_ms" 'BEGIN { printf "%.3f", ms / 1000 }')"
  sleep "$sleep_seconds"
}

# MIN_OBSERVER_BUDGET_MS is the smallest query-timeout budget this script
# will ever hand to an observer call. Below this floor, an observer
# invocation's own overhead makes success structurally implausible, so
# attempting it anyway would burn the last of the remaining time on a call
# that was never going to finish -- the correct action is to treat the
# remaining window as already exhausted for observation purposes and
# report unconfirmed immediately, not to launch a doomed probe.
#
# Calibrated against --psql-via-docker-network mode specifically, since
# that is what X99 CD runners use (no host psql; see quiescence.mjs):
# `docker run --rm postgres:16-alpine psql ...` alone (client startup +
# container create/start/teardown, before any SQL executes) measured
# consistently around 450ms on X99 with a warm image cache, independent
# of query complexity. A floor below that would make nearly every real
# call under this mode look like a "budget exhausted, skip" even when the
# actual deadline still has meaningful time left. Direct-psql mode (no
# Docker) has no such floor requirement and will simply succeed faster
# whenever this floor is met.
MIN_OBSERVER_BUDGET_MS=600

# run_node_bounded wraps an ENTIRE `node ...` invocation — including Node's
# own process startup and CLI-argument parsing, not just the SQL/timeout
# logic quiescence.mjs installs once it starts running — in an external
# `timeout -s KILL`, computed from ACTUAL remaining milliseconds against
# the given absolute deadline at the moment this function is called.
#
# This closes the exact gap Sol found: quiescence.mjs's own internal
# `timeout -s KILL` wrapper (in runPsql) is only installed after `node`
# has already started, loaded modules, and parsed argv — none of which is
# free, and none of which was previously inside any clamp at all. A single
# fixed budget handed to --query-timeout-ms bounded only the time *after*
# Node was already running, so the true wall-clock cost of a call could
# exceed the sampled remaining time by Node's own startup latency.
#
# Usage: run_node_bounded <deadline_epoch> -- <node args...>
# Exit codes: whatever the wrapped `node` invocation would normally
# return, OR 2 if there is not enough budget left even to attempt the
# call (mirroring quiescence.mjs's own "observation failed/unknown" exit
# code), OR 124/137 if the external timeout itself had to intervene
# (Node started but did not finish in time) -- callers must treat any
# non-zero exit here identically to a quiescence.mjs-reported failure,
# never as a special "the wrapper, not the tool, timed out" case.
run_node_bounded() {
  local deadline_arg="$1"; shift
  if [ "$1" != "--" ]; then
    echo "che372-d2: run_node_bounded internal usage error (missing --)" >&2
    return 2
  fi
  shift
  local budget_ms
  budget_ms="$(remaining_ms_before "$deadline_arg")"
  if [ "$budget_ms" -lt "$MIN_OBSERVER_BUDGET_MS" ]; then
    echo "che372-d2: skipped node invocation — only ${budget_ms}ms remain before the absolute deadline, below the ${MIN_OBSERVER_BUDGET_MS}ms floor Node startup plus a probe needs to structurally complete" >&2
    return 2
  fi
  local budget_seconds
  budget_seconds="$(awk -v ms="$budget_ms" 'BEGIN { printf "%.3f", ms / 1000 }')"
  timeout -s KILL "${budget_seconds}s" node "$@"
}

# confirm_no_live_session_for_pid proves — or fails to prove — that the
# EXACT recorded (pid, backend_start) session is gone AND that no other
# migrator-attributable (same role/database) session remains, via
# deploy/cd/quiescence.mjs's verify-live-session-is-absent, which queries
# pg_stat_activity directly for that identity rather than comparing
# against whatever a separate, unrelated LIMIT-1 query happens to return.
#
# The ENTIRE `node ...` invocation — Node's own startup and CLI parsing
# included, not just quiescence.mjs's internal SQL/timeout logic — runs
# through run_node_bounded, which computes actual remaining milliseconds
# against deadline_arg immediately before launching and wraps the whole
# call in an external `timeout -s KILL`. This is the fix for the defect
# Sol found across two rounds: a fixed --query-timeout-ms bounded only
# quiescence.mjs's own internal work, which starts running only after
# Node has already paid its startup cost — that cost sat entirely outside
# any previous clamp.
#
# --query-timeout-ms is still passed to quiescence.mjs, deliberately
# smaller than the outer run_node_bounded budget: quiescence.mjs's own
# internal timeout should fire first under normal conditions (producing a
# clean, attributable "observation query exceeded wall-clock timeout"
# error), with the outer timeout -s KILL only as the backstop that also
# catches anomalously slow Node startup itself.
#
# Exit code contract (matches quiescence.mjs's own three-way CLI
# contract, extended by run_node_bounded's pre-launch denial and the
# outer timeout's own escalation): 0 = confirmed absent; any non-zero —
# confirmed still live, an observation failure, a denied-before-launch
# budget exhaustion, or the outer timeout itself firing — is treated
# identically as "not confirmed," so a bare
# `if confirm_no_live_session_for_pid` check can never mistake "we don't
# know" for "it's gone."
confirm_no_live_session_for_pid() {
  local pg_pid="$1"
  local pg_backend_start="$2"
  local deadline_arg="$3"
  local budget_ms
  budget_ms="$(remaining_ms_before "$deadline_arg")"
  # inner_budget_ms is deliberately a bit smaller than the outer
  # run_node_bounded wrapper's own budget (recomputed fresh inside that
  # function from the same deadline_arg) so quiescence.mjs's own timeout
  # has room to fire and report cleanly before the outer KILL would.
  local inner_budget_ms=$((budget_ms > 200 ? budget_ms - 100 : budget_ms))
  local absent_result
  absent_result="$(run_node_bounded "$deadline_arg" -- deploy/cd/quiescence.mjs verify-live-session-is-absent "${quiescence_args_common[@]}" \
    --pid "$pg_pid" --backend-start "$pg_backend_start" \
    --role-name "$role_name" --database-name "$database_name" --query-timeout-ms "$inner_budget_ms" 2>&1)"
  local status=$?
  if [ "$status" -ne 0 ]; then
    echo "che372-d2: absence check for pid=$pg_pid did not confirm absence (exit $status): $absent_result" >&2
  fi
  return "$status"
}

# --- launch the migrator immediately; the watchdog loop below governs the
# ENTIRE remaining sequence (live-session discovery, running, cancellation,
# confirmation) under the one absolute deadline. No sub-step here has its
# own separate, unbounded budget. ---
"$migrate_binary" up &
migrator_os_pid=$!
echo "che372-d2: launched migrator os_pid=$migrator_os_pid" >&2

migrator_pg_pid=""
migrator_pg_backend_start=""
final_state=""   # "success" | "failed_nonzero" | "deadline_exceeded" | "needs_operator"

while :; do
  remaining="$(remaining_seconds)"
  if [ "$remaining" -le 0 ]; then
    final_state="deadline_exceeded"
    break
  fi

  if ! kill -0 "$migrator_os_pid" 2>/dev/null; then
    # OS process has already exited. Reap it and check its status; this
    # does not by itself prove the Postgres session is gone (a crashed
    # client can leave server-side work in flight), so still discover and
    # confirm the Postgres session below before declaring success.
    #
    # `wait` is a shell builtin whose own exit status IS the child's exit
    # status, so under `set -e` a nonzero migrator exit would abort this
    # script right here — before migrate_status is captured, before
    # final_state is set to failed_nonzero, before any backend-disappearance
    # confirmation runs. `set +e`/`set -e` bracket exactly this one command
    # so a nonzero child exit is captured as data, not treated as this
    # script's own failure.
    set +e
    wait "$migrator_os_pid" 2>/dev/null
    migrate_status=$?
    set -e
    if [ "$migrate_status" -ne 0 ]; then
      final_state="failed_nonzero"
      break
    fi
    final_state="success"
    break
  fi

  # Discover the migrator's live Postgres backend, within the same
  # deadline-governed loop — no separate 25-attempt sub-loop with its own
  # budget. A single bounded probe per iteration; missing this iteration
  # just means we check again next iteration, still under the same clock.
  # Every node invocation here — including the tiny JSON-field-extraction
  # ones — goes through run_node_bounded, so Node's own startup is inside
  # the clamp for every one of them, not just the SQL-facing calls.
  if [ -z "$migrator_pg_pid" ]; then
    discovery_budget_ms="$(remaining_ms_before "$work_deadline_epoch")"
    if [ "$discovery_budget_ms" -ge "$MIN_OBSERVER_BUDGET_MS" ]; then
      inner_discovery_budget_ms=$((discovery_budget_ms > 200 ? discovery_budget_ms - 100 : discovery_budget_ms))
      probe="$(run_node_bounded "$work_deadline_epoch" -- deploy/cd/quiescence.mjs find-live-session-by-role "${quiescence_args_common[@]}" \
        --role-name "$role_name" --database-name "$database_name" --query-timeout-ms "$inner_discovery_budget_ms" 2>/dev/null)" || probe=""
      if [ -n "$probe" ]; then
        candidate_pid="$(run_node_bounded "$work_deadline_epoch" -- -e 'try{process.stdout.write(String(JSON.parse(process.argv[1]).pid))}catch(e){process.stdout.write("")}' "$probe" 2>/dev/null || true)"
        candidate_backend_start="$(run_node_bounded "$work_deadline_epoch" -- -e 'try{process.stdout.write(String(JSON.parse(process.argv[1]).backendStart))}catch(e){process.stdout.write("")}' "$probe" 2>/dev/null || true)"
        if [ -n "$candidate_pid" ]; then
          cover_budget_ms="$(remaining_ms_before "$work_deadline_epoch")"
          if [ "$cover_budget_ms" -ge "$MIN_OBSERVER_BUDGET_MS" ]; then
            inner_cover_budget_ms=$((cover_budget_ms > 200 ? cover_budget_ms - 100 : cover_budget_ms))
            covered="$(run_node_bounded "$work_deadline_epoch" -- deploy/cd/quiescence.mjs verify-live-session-is-covered "${quiescence_args_common[@]}" \
              --pid "$candidate_pid" --backend-start "$candidate_backend_start" \
              --role-name "$role_name" --database-name "$database_name" --query-timeout-ms "$inner_cover_budget_ms" 2>&1)" && {
              migrator_pg_pid="$candidate_pid"
              migrator_pg_backend_start="$candidate_backend_start"
              echo "che372-d2: confirmed live migrator Postgres session pid=$migrator_pg_pid identity: $covered" >&2
            }
          fi
        fi
      fi
    fi
  fi

  sleep_clamped_to 200 "$work_deadline_epoch"
done

if [ "$final_state" = "success" ]; then
  # Confirmed OS exit zero within budget. Still require server-side proof
  # before claiming success — this block must resolve to success ONLY on
  # an actual confirmed absence, and to needs_operator for every other
  # case, with no path that silently falls through and leaves
  # final_state untouched:
  #   - never discovered a Postgres session at all (e.g. it finished so
  #     fast between poll iterations that this script never observed it
  #     live) — UNCERTAIN, not success, because there is nothing to have
  #     confirmed absent;
  #   - discovered a session but the absence check FAILED (observer
  #     error/timeout, unknown state) — UNCERTAIN, never treated as "no
  #     news is good news";
  #   - discovered a session and it is confirmed still live, or another
  #     migrator-attributable session remains — UNCERTAIN.
  # Only "discovered, and confirmed absent" keeps final_state=success.
  if [ -z "$migrator_pg_pid" ]; then
    echo "che372-d2: migrator os_pid=$migrator_os_pid exited 0 but no Postgres session was ever positively identified for it — cannot confirm server-side completion, treating as UNCERTAIN, not success" >&2
    final_state="needs_operator"
  elif ! confirm_no_live_session_for_pid "$migrator_pg_pid" "$migrator_pg_backend_start" "$work_deadline_epoch"; then
    echo "che372-d2: migrator os_pid=$migrator_os_pid exited 0 but its Postgres session pid=$migrator_pg_pid could not be confirmed absent — treating as UNCERTAIN, not success" >&2
    final_state="needs_operator"
  fi
fi

if [ "$final_state" = "success" ]; then
  final_now_epoch="$(date +%s)"
  if [ "$final_now_epoch" -gt "$work_deadline_epoch" ]; then
    echo "che372-d2: migrator completed but confirmation observed after work_deadline — treat as UNCERTAIN per spec, not a clean success" >&2
    exit 3
  fi
  echo "che372-d2: migration completed successfully within work_deadline (finished at $final_now_epoch, deadline $work_deadline_epoch)" >&2
  exit 0
fi

if [ "$final_state" = "failed_nonzero" ]; then
  echo "che372-d2: migrator exited non-zero before deadline — state UNCERTAIN, compare full ledger/object state before any decision" >&2
  exit 1
fi

# final_state is "deadline_exceeded" or "needs_operator" from the success
# check above: cancel the OS process if still running, and — this is the
# server-side proof the previous version never obtained — poll
# pg_stat_activity until the migrator's identified Postgres backend is
# actually gone, not merely until the OS process disappears.
#
# The cancellation attempt is bounded by the ORIGINAL absolute
# `deadline_epoch` computed at the top of this script, never by a fresh
# `now + reserve_seconds` clock. work_deadline_epoch = deadline_epoch -
# reserve_seconds specifically carves the reserve out of the same
# envelope; by the time this path runs, `now` is typically already past
# work_deadline_epoch (that is why we are here), so restarting a
# reserve_seconds-long countdown from "now" would silently extend the
# approved envelope every time cancellation itself takes any time to
# notice the deadline passed. Clamping to the pre-existing deadline_epoch
# means the reserve is spent once, not re-granted.
echo "che372-d2: entering cancellation path (${final_state}) for os_pid=$migrator_os_pid pg_pid=${migrator_pg_pid:-unknown}" >&2
if kill -0 "$migrator_os_pid" 2>/dev/null; then
  kill -TERM "$migrator_os_pid" 2>/dev/null || true
fi

cancel_deadline_epoch="$deadline_epoch"
os_confirmed=false
pg_confirmed=false
while [ "$(date +%s)" -lt "$cancel_deadline_epoch" ]; do
  if ! $os_confirmed && ! kill -0 "$migrator_os_pid" 2>/dev/null; then
    wait "$migrator_os_pid" 2>/dev/null || true
    os_confirmed=true
  fi
  if [ -n "$migrator_pg_pid" ]; then
    if ! $pg_confirmed && confirm_no_live_session_for_pid "$migrator_pg_pid" "$migrator_pg_backend_start" "$cancel_deadline_epoch"; then
      pg_confirmed=true
    fi
  else
    # Never discovered a Postgres session for this attempt — cannot prove
    # server-side absence of something we never identified. This is
    # exactly the "unknown state fails closed" rule: it does NOT count as
    # confirmed, it stays unconfirmed until the cancel deadline expires.
    pg_confirmed=false
  fi
  if $os_confirmed && $pg_confirmed; then
    break
  fi
  sleep_clamped_to 100 "$cancel_deadline_epoch"
done

# From here on, the absolute `deadline_epoch` is a HARD boundary on
# WAITING/CONFIRMING: no further poll loop or absence probe may START
# once it has passed. This closes the exact gap Sol found: the SIGKILL
# confirmation loop (up to five 100ms checks) and the one extra absence
# probe below previously ran unconditionally after the cancellation loop
# timed out, letting real wall-clock work continue past the approved
# envelope while the comments claimed the whole path was bounded by it.
#
# Sending SIGKILL itself is different: it is a single, effectively
# instantaneous syscall, not wall-clock work, and it is also the last
# safety action available — skipping it because the clock already reads
# deadline_epoch would leave a still-running process with no further
# attempt to stop it, which is strictly worse than a late confirmation.
# So the kill is unconditional (bounded only by whether it is still
# needed at all); only the loop that waits to CONFIRM it worked is
# deadline-gated, and confirmation is denied rather than assumed if that
# window is already gone.
if ! $os_confirmed && kill -0 "$migrator_os_pid" 2>/dev/null; then
  echo "che372-d2: graceful cancel unconfirmed for os_pid=$migrator_os_pid; escalating to SIGKILL on recorded pid" >&2
  kill -KILL "$migrator_os_pid" 2>/dev/null || true
  while [ "$(date +%s)" -lt "$deadline_epoch" ]; do
    kill -0 "$migrator_os_pid" 2>/dev/null || { os_confirmed=true; wait "$migrator_os_pid" 2>/dev/null || true; break; }
    sleep_clamped_to 100 "$deadline_epoch"
  done
fi

# confirm_no_live_session_for_pid checks its own remaining-time budget
# against deadline_epoch internally (see MIN_OBSERVER_BUDGET_MS above) and
# denies without spawning anything if too little time remains, so no
# separate outer clock check is needed here to keep this call from
# starting a doomed probe.
if [ -n "$migrator_pg_pid" ] && ! $pg_confirmed; then
  if confirm_no_live_session_for_pid "$migrator_pg_pid" "$migrator_pg_backend_start" "$deadline_epoch"; then
    pg_confirmed=true
  fi
fi

# A never-identified Postgres session can never count as confirmed absent
# — this is the fail-closed default for "we don't know," not a special
# case: pg_confirmed only becomes true via an actual observed absence
# above, and starts and stays false here when migrator_pg_pid is empty.
# Reaching the deadline with anything unconfirmed is itself the
# deterministic non-success outcome — this script never waits past
# deadline_epoch hoping for a late confirmation.
if $os_confirmed && $pg_confirmed; then
  echo "che372-d2: os_pid=$migrator_os_pid and Postgres pid=$migrator_pg_pid both confirmed terminated after ${final_state} — needs_operator (state uncertain: hooks or earlier statements may have committed)" >&2
else
  echo "che372-d2: termination UNCONFIRMED at or after the absolute deadline (os_confirmed=$os_confirmed pg_confirmed=$pg_confirmed pg_pid=${migrator_pg_pid:-never-identified}) — needs_operator, fence must stay up" >&2
fi
exit 3
