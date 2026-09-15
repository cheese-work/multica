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

# monotonic_ms returns milliseconds since boot, read from /proc/uptime's
# first (monotonic) field. Unlike `date +%s`/`date +%s%3N` (wall-clock,
# CLOCK_REALTIME), this is unaffected by a backward wall-clock step (NTP
# correction, manual clock change, leap-second smear) — a wall-clock
# read can jump backward mid-run, which would let remaining_seconds()
# silently report MORE time than actually remains and extend the
# approved envelope. /proc/uptime is Linux-specific but this script's
# only supported runtime is X99 CD runners / the CD container image,
# both Linux. All budget/deadline arithmetic below is anchored to this
# clock; wall-clock `date +%s` is retained ONLY for human-readable
# correlation in log lines (e.g. the work_deadline= value tests parse),
# never for a gating decision.
monotonic_ms() {
  awk '{ printf "%d", $1 * 1000 }' /proc/uptime
}

usage() {
  cat <<'EOF'
usage: migrate-supervised.sh --database-url URL --migrate-binary PATH
  --role-name ROLE --database-name DBNAME
  [--observer-database-url URL] [--psql-via-docker-network NAME]
  [--cutover-started-epoch SECONDS] [--migration-allocation-seconds N]
  [--reserve-seconds N] [--statement-timeout-ms N] [--lock-timeout-ms N]
  [--decision-file PATH --attempt-id ID]

--decision-file/--attempt-id, when both given, make this script write a
durable record of every terminal outcome (not just success) to PATH, so a
crash mid-run or an external restart leaves something to reconcile
against instead of silently losing track — and, on a confirmed success,
write the exact "starting_candidate\n<attempt-id>" contract
docker/entrypoint.cd.sh already waits on and verifies for freshness. Both
flags are optional together (a standalone/test invocation with neither
still runs the full migration/watchdog logic; passing only one is a usage
error, since a decision file without a bound attempt id could be read as
current by an unrelated attempt).

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
  4   refused to start: --decision-file already records a TERMINAL
      disposition from a previous attempt. Overwriting it would destroy the
      only durable evidence of that attempt's outcome and let a partially
      migrated database be retried under a fresh budget. An operator must
      inspect and remove/rotate the file before another attempt.
  5   migration ran and the migrator exited zero, but a post-migration
      object-validity check failed (an INVALID index left behind by a
      concurrent index build that silently no-op'd or failed). Exit zero
      alone does not prove the intended objects exist and are usable, so
      this is needs_operator, never starting_candidate.
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
decision_file=""
attempt_id=""

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
    --decision-file) decision_file=${2:?}; shift 2 ;;
    --attempt-id) attempt_id=${2:?}; shift 2 ;;
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
if [ -n "$decision_file" ] || [ -n "$attempt_id" ]; then
  [ -n "$decision_file" ] || { echo "--attempt-id given without --decision-file" >&2; usage >&2; exit 2; }
  [ -n "$attempt_id" ] || { echo "--decision-file given without --attempt-id" >&2; usage >&2; exit 2; }
fi

# write_decision durably records a terminal (or in-progress) outcome for
# this attempt, so a crash mid-script or an external restart has something
# to reconcile against instead of silently losing track (Astra's exact
# "crash reconciliation is not connected to the runner/entrypoint decision"
# gap). Writes are atomic (write to a tmp file in the same directory, then
# rename) so a reader — including entrypoint.cd.sh's own poll loop — never
# observes a half-written file. The first line is the decision value; the
# second is this run's attempt id, mirroring the exact two-line contract
# docker/entrypoint.cd.sh already parses and freshness-checks. When
# --decision-file/--attempt-id were not supplied (e.g. standalone/test
# invocations) this is a deliberate no-op — nothing to write to and no
# entrypoint is waiting on this contract in that mode.
write_decision() {
  local decision="$1"
  [ -n "$decision_file" ] || return 0
  local tmp_file
  tmp_file="${decision_file}.tmp.$$"
  {
    printf '%s\n' "$decision"
    printf '%s\n' "$attempt_id"
  } >"$tmp_file"
  mv -f "$tmp_file" "$decision_file"
  echo "che372-d2: recorded decision=$decision attempt_id=$attempt_id at $decision_file" >&2
}

# Decision-file preservation gate. write_decision "migration_started"
# below is the FIRST thing this script records, and it used to run
# unconditionally — silently clobbering whatever was already at
# $decision_file, including a TERMINAL failure disposition
# (needs_operator_*, denied_*, failed_confirmed_terminated) recorded by an
# earlier attempt at the same path. That is exactly the reconciliation hole
# Astra named: a second invocation could erase the proof that a previous
# attempt left the database in an uncertain, partially-migrated state, then
# retry it under a renewed 15s/40s budget as if nothing had happened.
#
# This script is deliberately conservative everywhere else (it "does not
# talk to C00 and accepts no production credentials"; every unknown state
# fails closed), so the same discipline applies here: a recorded terminal
# disposition is evidence, and evidence is not this script's to destroy.
# Refuse to proceed and exit 4, leaving the file byte-for-byte untouched
# for an operator to inspect.
#
# What is NOT terminal: "migration_started" itself. That is a progress
# marker, not a disposition — a crashed previous run that got no further
# than writing it recorded no outcome to preserve, and blocking on it would
# make any crash permanently unrecoverable without manual file surgery. An
# absent file is likewise a clean slate. Everything else is terminal,
# including a terminal SUCCESS (starting_candidate): re-running a migration
# whose success was already published to the entrypoint is its own hazard,
# and the attempt id being the same or different does not change that — a
# same-id rerun is a duplicate attempt and a different-id rerun is a fresh
# attempt reusing a dirty path. Both need an operator, not a silent
# overwrite.
#
# The decision value is read from the file's FIRST line, matching
# write_decision's own two-line ("<decision>\n<attempt id>") contract and
# docker/entrypoint.cd.sh's own parse of the same file.
if [ -n "$decision_file" ] && [ -f "$decision_file" ]; then
  existing_decision="$(sed -n '1p' "$decision_file" 2>/dev/null || true)"
  existing_attempt_id="$(sed -n '2p' "$decision_file" 2>/dev/null || true)"
  if [ "$existing_decision" != "migration_started" ]; then
    echo "che372-d2: refusing to start — $decision_file already records a terminal disposition from a previous attempt (decision='$existing_decision' attempt_id='$existing_attempt_id'). Overwriting it would destroy the only durable evidence of that attempt's outcome and allow a partially migrated database to be retried under a fresh budget. An operator must inspect and remove/rotate this file before another attempt." >&2
    exit 4
  fi
  echo "che372-d2: $decision_file records non-terminal progress (decision='$existing_decision' attempt_id='$existing_attempt_id') from an attempt that recorded no outcome — proceeding and taking over the file" >&2
fi

write_decision "migration_started"

migration_started_epoch="$(date +%s)"       # wall-clock, log/correlation only
migration_started_monotonic_ms="$(monotonic_ms)"

# migration_deadline = min(migration_started + allocation, cutover_started + 40)
# The 40s bound is v3's fixed constant (5+5+15+15 = 40s through the end of
# the migration phase in the 60s envelope); it is not a CLI flag because
# changing it would silently widen the approved envelope.
#
# All arithmetic that actually GATES the budget (deadline_monotonic_ms /
# work_deadline_monotonic_ms below) is computed on the monotonic clock.
# deadline_epoch/work_deadline_epoch (wall-clock) are still computed and
# logged, unchanged in format, purely so operators/tests can correlate a
# run against real wall-clock time (e.g. `work_deadline=<epoch>` is parsed
# by test-migrate-supervised.sh) — they are never read by a gating check.
cutover_bound_seconds=40
deadline_epoch=$((migration_started_epoch + migration_allocation_seconds))
deadline_monotonic_ms=$((migration_started_monotonic_ms + migration_allocation_seconds * 1000))
if [ -n "$cutover_started_epoch" ]; then
  cutover_deadline=$((cutover_started_epoch + cutover_bound_seconds))
  if [ "$cutover_deadline" -lt "$deadline_epoch" ]; then
    # The cutover bound is expressed by the caller in wall-clock epoch
    # seconds (it has no monotonic meaning across process boundaries), so
    # convert it to a monotonic-ms deadline via its offset from "now" at
    # the moment this script observed both clocks — this is the one place
    # a wall-clock READING influences the monotonic budget, but only as a
    # one-time offset captured now, not as an ongoing comparison, so a
    # later backward wall-clock step cannot retroactively change it.
    cutover_deadline_monotonic_ms=$((migration_started_monotonic_ms + (cutover_deadline - migration_started_epoch) * 1000))
    deadline_epoch="$cutover_deadline"
    if [ "$cutover_deadline_monotonic_ms" -lt "$deadline_monotonic_ms" ]; then
      deadline_monotonic_ms="$cutover_deadline_monotonic_ms"
    fi
  fi
fi
work_deadline_epoch=$((deadline_epoch - reserve_seconds))
work_deadline_monotonic_ms=$((deadline_monotonic_ms - reserve_seconds * 1000))

remaining_seconds() {
  local now_ms
  now_ms="$(monotonic_ms)"
  echo $(( (work_deadline_monotonic_ms - now_ms) / 1000 ))
}

if [ "$(remaining_seconds)" -le 0 ]; then
  echo "che372-d2: usable work time already expired before migration start (deny entry)" >&2
  write_decision "denied_expired_before_start"
  exit 3
fi

# work_deadline_monotonic_ms is the AUTHORITATIVE enforced boundary — it is
# what remaining_seconds(), remaining_ms_before() and every loop condition
# in this script actually compare against. work_deadline (epoch seconds) is
# a floored wall-clock rendering of the same instant and can sit up to 999ms
# earlier than the real boundary, because migration_started_epoch floors
# `date +%s` while migration_started_monotonic_ms does not.
#
# Both are logged in a machine-parseable form deliberately: a test that
# wants to assert this script exited by its real deadline must bind to
# work_deadline_monotonic_ms and read /proc/uptime itself the same way
# monotonic_ms() does (monotonic-to-monotonic, no rounding on either side).
# Binding such an assertion to the floored epoch value instead re-introduces
# up to a full second of slack and stops measuring the enforcement boundary
# at all.
echo "che372-d2: migration_started=$migration_started_epoch work_deadline=$work_deadline_epoch work_deadline_monotonic_ms=$work_deadline_monotonic_ms deadline_monotonic_ms=$deadline_monotonic_ms remaining=$(remaining_seconds)s (monotonic clock governs enforcement; wall-clock values are for correlation only)" >&2

statement_timeout_ms_capped=$(( statement_timeout_ms < ($(remaining_seconds) * 1000) ? statement_timeout_ms : ($(remaining_seconds) * 1000) ))
if [ "$statement_timeout_ms_capped" -le 0 ]; then
  echo "che372-d2: statement timeout rounds to <=0ms (disabled in Postgres) — deny" >&2
  write_decision "denied_statement_timeout_zero"
  exit 3
fi
if [ "$lock_timeout_ms" -le 0 ]; then
  echo "che372-d2: lock timeout rounds to <=0ms (disabled in Postgres) — deny" >&2
  write_decision "denied_lock_timeout_zero"
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
# the given absolute deadline, expressed in MONOTONIC milliseconds (see
# monotonic_ms above — every caller in this script passes one of
# work_deadline_monotonic_ms/cancel_deadline_monotonic_ms/
# deadline_monotonic_ms, never a wall-clock value), floored at 0 — never
# negative, so callers can compare directly against a minimum floor
# without a separate sign check. A backward wall-clock adjustment cannot
# change this result.
remaining_ms_before() {
  local deadline_monotonic_ms="$1"
  local now_ms
  now_ms="$(monotonic_ms)"
  local remaining=$((deadline_monotonic_ms - now_ms))
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
# Usage: run_node_bounded <deadline_monotonic_ms> -- <node args...>
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

# run_final_gate calls quiescence.mjs's `final-gate` subcommand — the
# zero-tolerance check (open transactions/snapshots, prepared xacts,
# advisory locks, unaccounted locks, unfenced idle sessions) — through the
# same run_node_bounded clamp as every other observer call.
#
# fenced_pids is a comma-separated RAW pid allowlist (may be empty), used
# only where verification is structurally impossible — i.e. the pre-launch
# gate, where the migrator has not started and owns no session to verify.
#
# fenced_sessions is the allowlist that matters once the migrator is live:
# a comma-separated list of "<pid>|<backend_start>" pairs drawn from
# $migrator_verified_sessions, the registry of sessions THIS script
# discovered and independently confirmed via
# quiescence.mjs verify-live-session-is-covered. quiescence.mjs re-verifies
# every pair against pg_stat_activity at gate time before exempting it, so
# a recycled PID or a pair that no longer matches the expected
# role/database is simply not fenced.
#
# A previous version instead passed --fenced-role/--fenced-database, which
# told the gate to exempt EVERY session currently authenticated as this
# role on this database. That is not ownership — it is "whoever holds these
# credentials right now" — so an unrelated foreign client on the migration
# role was exempted from every quiescence check and could transact
# undetected for the entire migration window, with backend_start discarded
# so not even PID reuse was guarded. Session identity in this controller is
# always the unforgeable (pid, backend_start) pair, established by
# discovery + verification, never inferred from a shared role name. Do not
# reintroduce a role-shaped fence here.
#
# Exit code contract: 0 = admitted (gate passed); any non-zero — denied,
# an observation failure, or a pre-launch budget denial from
# run_node_bounded — is "not admitted," identically to
# confirm_no_live_session_for_pid's fail-closed contract. Callers never
# distinguish "gate said no" from "we couldn't tell."
run_final_gate() {
  local deadline_arg="$1"
  local fenced_pids="$2"
  local fenced_sessions="${3:-}"
  local budget_ms
  budget_ms="$(remaining_ms_before "$deadline_arg")"
  local inner_budget_ms=$((budget_ms > 200 ? budget_ms - 100 : budget_ms))
  local gate_args=(deploy/cd/quiescence.mjs final-gate "${quiescence_args_common[@]}" \
    --query-timeout-ms "$inner_budget_ms")
  if [ -n "$fenced_pids" ]; then
    gate_args+=(--fenced-pids "$fenced_pids")
  fi
  if [ -n "$fenced_sessions" ]; then
    gate_args+=(--fenced-sessions "$fenced_sessions" --role-name "$role_name" --database-name "$database_name")
  fi
  local gate_result
  gate_result="$(run_node_bounded "$deadline_arg" -- "${gate_args[@]}" 2>&1)"
  local status=$?
  if [ "$status" -ne 0 ]; then
    echo "che372-d2: final-gate denied (fenced-pids=${fenced_pids:-none}, fenced-sessions=${fenced_sessions:-none}, exit $status): $gate_result" >&2
  fi
  return "$status"
}

# find_invalid_indexes reports any index left INVALID in the target
# database, via quiescence.mjs's existing observer plumbing (same bounded,
# autocommit, metadata-only psql path as every other check here — no new
# connection mechanism, and nothing that mutates).
#
# Why exit zero from the migrator is not enough: PostgreSQL's CREATE INDEX
# CONCURRENTLY builds in multiple passes and, on failure, leaves the index
# behind with pg_index.indisvalid = false — present in the catalog, named,
# and useless to the planner. This repo REQUIRES every migration index to
# be built concurrently and, because a conditionally skipped migration is
# still recorded in schema_migrations, requires later migrations to use
# IF NOT EXISTS (see CLAUDE.md's migration rules). Together those mean an
# invalid index is silently tolerated end-to-end: the concurrent build
# fails, the ledger records the migration as applied, a later IF NOT EXISTS
# sees the invalid index as "already there" and skips, and the migrator
# exits zero. The candidate would then be admitted onto a schema whose
# indexes do not exist for query planning purposes.
#
# Echoes the offending index names on stdout (one per line) and returns 0
# only when the check RAN and found nothing. A non-zero return means either
# "found invalid indexes" or "could not tell" — the caller must treat both
# as failure, matching this script's fail-closed contract everywhere else.
# Status is distinguished on stdout so the caller can log precisely.
find_invalid_indexes() {
  local deadline_arg="$1"
  local budget_ms
  budget_ms="$(remaining_ms_before "$deadline_arg")"
  if [ "$budget_ms" -lt "$MIN_OBSERVER_BUDGET_MS" ]; then
    echo "unknown: only ${budget_ms}ms remain, below the ${MIN_OBSERVER_BUDGET_MS}ms observer floor"
    return 1
  fi
  local inner_budget_ms=$((budget_ms > 200 ? budget_ms - 100 : budget_ms))
  # list-invalid-indexes exits 1 BOTH when it found invalid indexes and
  # when the observation failed, so the exit code alone cannot distinguish
  # them — the JSON body can. Capture the output either way (`|| true`) and
  # let the parse below decide; an unparseable/ok:false body falls through
  # to the "unknown" branch, which the caller treats as failure just the
  # same.
  local result
  result="$(run_node_bounded "$deadline_arg" -- deploy/cd/quiescence.mjs list-invalid-indexes "${quiescence_args_common[@]}" \
    --query-timeout-ms "$inner_budget_ms" 2>&1)" || true
  if [ -z "$result" ]; then
    echo "unknown: list-invalid-indexes produced no output (observer failed or was killed before writing a result)"
    return 1
  fi
  local invalid
  # Emits "NONE" for a clean ok:true result, the comma-joined index names
  # for an ok:true result with findings, and "UNKNOWN" for ok:false or any
  # unparseable body. An empty/ambiguous result never renders as "clean".
  invalid="$(run_node_bounded "$deadline_arg" -- -e 'try{const r=JSON.parse(process.argv[1]);if(r&&r.ok===true&&Array.isArray(r.invalidIndexes)){process.stdout.write(r.invalidIndexes.length?r.invalidIndexes.join(","):"NONE")}else{process.stdout.write("UNKNOWN")}}catch(e){process.stdout.write("UNKNOWN")}' "$result" 2>/dev/null)" || {
    echo "unknown: could not parse invalid-index result: $result"
    return 1
  }
  if [ "$invalid" = "NONE" ]; then
    return 0
  fi
  if [ "$invalid" = "UNKNOWN" ] || [ -z "$invalid" ]; then
    echo "unknown: could not parse invalid-index result: $result"
    return 1
  fi
  if [ -n "$invalid" ]; then
    echo "invalid: $invalid"
    return 1
  fi
  return 0
}

# discover_additional_verified_sessions extends $migrator_verified_sessions
# with any NEW same-role/database session that this script can independently
# verify, so the migrator's legitimate additional connections (a
# metadata/hook connection opened mid-run by its pgx pool) are fenced
# without ever fencing "whoever is on this role".
#
# The discipline is deliberately identical to the first session's: the role
# query only PROPOSES candidates; each candidate that is not already in the
# registry is then confirmed by verify-live-session-is-covered against its
# exact (pid, backend_start) pair before it is appended. A candidate that
# fails verification is left out — and, being unfenced, will make the very
# next final-gate sample deny if it is in fact touching the database. That
# is the correct direction to fail: a foreign session on the migration role
# does not become ours by appearing while we are running.
#
# Bounded work only: one role query plus at most one verification per
# iteration. There is no inner retry loop, and every node invocation runs
# through run_node_bounded against the same work deadline, so this cannot
# outlive the per-iteration budget. If more than one new candidate appears
# in the same iteration, the extras are simply picked up by later
# iterations (or, if they are genuinely foreign, denied by the gate) —
# draining an unbounded queue inside one iteration is exactly the kind of
# sub-loop with its own budget this script does not allow.
discover_additional_verified_sessions() {
  local deadline_arg="$1"
  local budget_ms
  budget_ms="$(remaining_ms_before "$deadline_arg")"
  if [ "$budget_ms" -lt "$MIN_OBSERVER_BUDGET_MS" ]; then
    return 0
  fi
  local inner_budget_ms=$((budget_ms > 200 ? budget_ms - 100 : budget_ms))
  local sessions_json
  sessions_json="$(run_node_bounded "$deadline_arg" -- deploy/cd/quiescence.mjs find-live-sessions-for-role "${quiescence_args_common[@]}" \
    --role-name "$role_name" --database-name "$database_name" --query-timeout-ms "$inner_budget_ms" 2>/dev/null)" || return 0
  [ -n "$sessions_json" ] || return 0

  # Render the role query's result as newline-separated "pid|backend_start"
  # candidate pairs. This is a pure text transform of data already fetched;
  # it decides nothing about trust.
  local candidate_budget_ms
  candidate_budget_ms="$(remaining_ms_before "$deadline_arg")"
  if [ "$candidate_budget_ms" -lt "$MIN_OBSERVER_BUDGET_MS" ]; then
    return 0
  fi
  local candidates
  candidates="$(run_node_bounded "$deadline_arg" -- -e 'try{const r=JSON.parse(process.argv[1]);if(r&&r.ok&&Array.isArray(r.sessions))process.stdout.write(r.sessions.map((s)=>`${s.pid}|${s.backendStart}`).join("\n"))}catch(e){}' "$sessions_json" 2>/dev/null)" || return 0
  [ -n "$candidates" ] || return 0

  local candidate
  while IFS= read -r candidate; do
    [ -n "$candidate" ] || continue
    # Already-registered pairs need no re-verification here: the final gate
    # itself re-verifies every registry entry on every sample, so a pair
    # that has gone stale stops being fenced there, not here.
    case ",${migrator_verified_sessions}," in
      *",${candidate},"*) continue ;;
    esac
    local candidate_pid="${candidate%%|*}"
    local candidate_backend_start="${candidate#*|}"
    [ -n "$candidate_pid" ] && [ -n "$candidate_backend_start" ] || continue
    local verify_budget_ms
    verify_budget_ms="$(remaining_ms_before "$deadline_arg")"
    if [ "$verify_budget_ms" -lt "$MIN_OBSERVER_BUDGET_MS" ]; then
      return 0
    fi
    local inner_verify_budget_ms=$((verify_budget_ms > 200 ? verify_budget_ms - 100 : verify_budget_ms))
    local covered
    if covered="$(run_node_bounded "$deadline_arg" -- deploy/cd/quiescence.mjs verify-live-session-is-covered "${quiescence_args_common[@]}" \
      --pid "$candidate_pid" --backend-start "$candidate_backend_start" \
      --role-name "$role_name" --database-name "$database_name" --query-timeout-ms "$inner_verify_budget_ms" 2>&1)"; then
      migrator_verified_sessions="${migrator_verified_sessions},${candidate}"
      echo "che372-d2: verified an additional migrator-attributable session pid=$candidate_pid backend_start=$candidate_backend_start: $covered" >&2
    else
      echo "che372-d2: candidate same-role session pid=$candidate_pid backend_start=$candidate_backend_start did NOT verify and is therefore NOT fenced: $covered" >&2
    fi
    # One verification attempt per iteration keeps this inside the
    # per-iteration budget; remaining candidates are handled next
    # iteration, or denied by the gate if they are not ours.
    return 0
  done <<EOF
$candidates
EOF
}

# Pre-launch final gate: refuse to start the migrator at all unless the
# database is already quiescent by the same zero-tolerance definition D2
# uses to decide the migration is DONE (evaluateFinalGate) — no fenced
# PIDs yet, since the migrator has not started and has no session to
# exempt. This is inside the same monotonic work-deadline budget as
# everything else; it is not a separate unbounded pre-check.
if ! run_final_gate "$work_deadline_monotonic_ms" ""; then
  echo "che372-d2: pre-launch final-gate did not admit — database is not quiescent, refusing to start migrator (deny entry)" >&2
  write_decision "denied_not_quiescent"
  exit 3
fi
echo "che372-d2: pre-launch final-gate admitted — database quiescent, starting migrator" >&2

# --- launch the migrator immediately; the watchdog loop below governs the
# ENTIRE remaining sequence (live-session discovery, running, cancellation,
# confirmation) under the one absolute deadline. No sub-step here has its
# own separate, unbounded budget. ---
"$migrate_binary" up &
migrator_os_pid=$!
echo "che372-d2: launched migrator os_pid=$migrator_os_pid" >&2

migrator_pg_pid=""
migrator_pg_backend_start=""
final_state=""   # "success" | "failed_nonzero" | "deadline_exceeded" | "needs_operator" | "fencing_violated"

# migrator_verified_sessions is the registry of Postgres sessions this
# script has DISCOVERED and INDEPENDENTLY VERIFIED as belonging to this
# migration attempt. Format: comma-separated "<pid>|<backend_start>"
# entries, exactly what quiescence.mjs final-gate --fenced-sessions parses.
#
# Every entry earns its place the same way, with no shortcut for the second
# and later ones: find-live-session-by-role (or findLiveSessionsForRole)
# proposes a CANDIDATE, and verify-live-session-is-covered confirms that
# exact (pid, backend_start) pair is live and running as the expected
# role/database before the pair is appended here. "It is on our role right
# now" is never sufficient — that is the admission hole the previous
# role-based fence had, and it is what the unforgeable-identity discipline
# documented in quiescence.mjs exists to prevent. The migrator legitimately
# opens more than one backend (its pinned migration connection plus
# metadata/hook connections that come and go), so this list grows over the
# run; it is a registry of proven identities, not a role query cached.
migrator_verified_sessions=""

# Continuous-fencing state. Once the migrator's own Postgres session is
# known, every loop iteration re-checks that NOTHING else is touching the
# database: final-gate --fenced-sessions <verified pairs> denies on any
# unaccounted session/lock/transaction other than this attempt's own
# verified ones.
#
# Latching, not tolerance: ANY denied/stale/unknown sample is immediately
# TERMINAL for the run. An earlier version allowed two consecutive misses
# before failing AND reset the counter to zero on the next successful
# sample — so a foreign transaction observed once could be forgotten
# entirely by the very next sample, and the run would go on to report
# success. A fencing violation is a statement about what was true at a
# moment in time; a later clean observation does not un-observe it. There
# is deliberately no transient-retry grace here: the denial path already
# covers observer failures, and treating "we could not tell" as a reason to
# keep going is the same fail-open this whole controller refuses elsewhere.
#
# fencing_max_sample_age_ms (1000ms) mirrors quiescence.mjs's SampleTracker
# maxSampleAgeMs: a gap wider than that between successive SUCCESSFUL
# samples means the database was unobserved for longer than the design
# allows, which is itself a coverage failure regardless of what the samples
# on either side said. The age is measured against the PREVIOUS successful
# sample's timestamp and evaluated BEFORE that timestamp is overwritten, so
# a single slow observation whose own duration exceeds the budget is caught
# rather than laundered into "age zero, I just succeeded."
fencing_last_sample_monotonic_ms=""
fencing_max_sample_age_ms=1000
# fencing_interval_ms is the target LOOP PERIOD, not a post-iteration sleep
# duration. The addendum budgets a sample every <=250ms; sleeping a flat
# 200ms AFTER an iteration that already spent time on bounded observer calls
# makes the real period 200ms + iteration duration, which can exceed 250ms
# on its own. The sleep at the bottom of the loop is therefore computed as
# max(0, fencing_interval_ms - iteration_duration_ms).
fencing_interval_ms=250

while :; do
  iteration_start_monotonic_ms="$(monotonic_ms)"
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
    discovery_budget_ms="$(remaining_ms_before "$work_deadline_monotonic_ms")"
    if [ "$discovery_budget_ms" -ge "$MIN_OBSERVER_BUDGET_MS" ]; then
      inner_discovery_budget_ms=$((discovery_budget_ms > 200 ? discovery_budget_ms - 100 : discovery_budget_ms))
      probe="$(run_node_bounded "$work_deadline_monotonic_ms" -- deploy/cd/quiescence.mjs find-live-session-by-role "${quiescence_args_common[@]}" \
        --role-name "$role_name" --database-name "$database_name" --query-timeout-ms "$inner_discovery_budget_ms" 2>/dev/null)" || probe=""
      if [ -n "$probe" ]; then
        candidate_pid="$(run_node_bounded "$work_deadline_monotonic_ms" -- -e 'try{process.stdout.write(String(JSON.parse(process.argv[1]).pid))}catch(e){process.stdout.write("")}' "$probe" 2>/dev/null || true)"
        candidate_backend_start="$(run_node_bounded "$work_deadline_monotonic_ms" -- -e 'try{process.stdout.write(String(JSON.parse(process.argv[1]).backendStart))}catch(e){process.stdout.write("")}' "$probe" 2>/dev/null || true)"
        if [ -n "$candidate_pid" ]; then
          cover_budget_ms="$(remaining_ms_before "$work_deadline_monotonic_ms")"
          if [ "$cover_budget_ms" -ge "$MIN_OBSERVER_BUDGET_MS" ]; then
            inner_cover_budget_ms=$((cover_budget_ms > 200 ? cover_budget_ms - 100 : cover_budget_ms))
            covered="$(run_node_bounded "$work_deadline_monotonic_ms" -- deploy/cd/quiescence.mjs verify-live-session-is-covered "${quiescence_args_common[@]}" \
              --pid "$candidate_pid" --backend-start "$candidate_backend_start" \
              --role-name "$role_name" --database-name "$database_name" --query-timeout-ms "$inner_cover_budget_ms" 2>&1)" && {
              migrator_pg_pid="$candidate_pid"
              migrator_pg_backend_start="$candidate_backend_start"
              # Seed the verified-session registry with this confirmed
              # pair. Reuse exactly the values verify-live-session-is-covered
              # just vouched for — never re-derive them from a fresh
              # role query, which would reintroduce the "shares a role
              # name" assumption this registry exists to avoid.
              migrator_verified_sessions="${migrator_pg_pid}|${migrator_pg_backend_start}"
              echo "che372-d2: confirmed live migrator Postgres session pid=$migrator_pg_pid identity: $covered" >&2
            }
          fi
        fi
      fi
    fi
  fi

  # Continuous fencing: once the migrator's own session is known, keep
  # proving no OTHER session/lock/transaction is touching the database on
  # every iteration — not just once at discovery time. This is the
  # direct-database-consumer fence the HTTP-only front door cannot see.
  if [ -n "$migrator_pg_pid" ]; then
    # Pick up any NEW migrator-attributable connection before sampling, so
    # a hook connection the pool just opened is verified and fenced on the
    # same iteration rather than denying the gate for one cycle first.
    # Unverifiable candidates are deliberately left unfenced — see this
    # function's comment.
    discover_additional_verified_sessions "$work_deadline_monotonic_ms"

    fencing_budget_ms="$(remaining_ms_before "$work_deadline_monotonic_ms")"
    if [ "$fencing_budget_ms" -ge "$MIN_OBSERVER_BUDGET_MS" ]; then
      if run_final_gate "$work_deadline_monotonic_ms" "" "$migrator_verified_sessions"; then
        # The sample SUCCEEDED. Its age must be judged before its
        # timestamp is recorded, and against the time the observation
        # actually FINISHED — not against the timestamp captured before the
        # (possibly slow) gate call ran.
        #
        # An earlier version did both of these backwards: it stamped
        # fencing_last_sample_monotonic_ms with a value read BEFORE the
        # call, then compared "now" against that freshly-written value, so
        # the computed age on a successful iteration was always ~zero. A
        # gate call that itself took longer than fencing_max_sample_age_ms
        # — leaving the database unobserved for that whole span — was
        # laundered into a clean sample. Comparing the completion instant
        # against the PREVIOUS successful sample catches exactly that.
        fencing_sample_done_ms="$(monotonic_ms)"
        if [ -n "$fencing_last_sample_monotonic_ms" ]; then
          fencing_sample_age_ms=$((fencing_sample_done_ms - fencing_last_sample_monotonic_ms))
          if [ "$fencing_sample_age_ms" -gt "$fencing_max_sample_age_ms" ]; then
            echo "che372-d2: continuous fencing violated — ${fencing_sample_age_ms}ms elapsed between successful fencing samples (max ${fencing_max_sample_age_ms}ms) while migrator pid=$migrator_pg_pid was running; the database was unobserved for longer than the design allows, so this sample's own success does not restore coverage" >&2
            final_state="fencing_violated"
            break
          fi
        fi
        fencing_last_sample_monotonic_ms="$fencing_sample_done_ms"
      else
        # LATCHING: one denied/stale/unknown sample is terminal. Not "two
        # consecutive", and no later successful sample can clear it —
        # run_final_gate already collapses "denied" and "could not tell"
        # into the same fail-closed non-zero, and neither is something a
        # subsequent clean observation retracts.
        echo "che372-d2: continuous fencing violated — a fencing sample was denied or could not be taken while migrator pid=$migrator_pg_pid was running (verified sessions: ${migrator_verified_sessions:-none}); this is terminal for the run, not retried" >&2
        final_state="fencing_violated"
        break
      fi
    else
      echo "che372-d2: skipped fencing sample — insufficient budget (${fencing_budget_ms}ms) this iteration" >&2
    fi
  fi

  # Maintain the <=250ms sampling PERIOD rather than adding a flat sleep on
  # top of however long this iteration's observer calls took. A fixed
  # `sleep 0.2` after a 300ms iteration yields a 500ms period, silently
  # exceeding the addendum's sampling budget; subtracting the iteration's
  # own duration keeps the period at the target whenever the work fits, and
  # yields a zero-length sleep (immediate next iteration) when it does not.
  # Still clamped to the absolute deadline by sleep_clamped_to.
  iteration_elapsed_ms=$(( $(monotonic_ms) - iteration_start_monotonic_ms ))
  iteration_sleep_ms=$(( fencing_interval_ms - iteration_elapsed_ms ))
  if [ "$iteration_sleep_ms" -gt 0 ]; then
    sleep_clamped_to "$iteration_sleep_ms" "$work_deadline_monotonic_ms"
  fi
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
  elif ! confirm_no_live_session_for_pid "$migrator_pg_pid" "$migrator_pg_backend_start" "$work_deadline_monotonic_ms"; then
    echo "che372-d2: migrator os_pid=$migrator_os_pid exited 0 but its Postgres session pid=$migrator_pg_pid could not be confirmed absent — treating as UNCERTAIN, not success" >&2
    final_state="needs_operator"
  fi
fi

if [ "$final_state" = "success" ]; then
  final_now_epoch="$(date +%s)"       # wall-clock, log correlation only
  final_now_monotonic_ms="$(monotonic_ms)"
  if [ "$final_now_monotonic_ms" -gt "$work_deadline_monotonic_ms" ]; then
    echo "che372-d2: migrator completed but confirmation observed after work_deadline — treat as UNCERTAIN per spec, not a clean success" >&2
    write_decision "denied_late_confirmation"
    exit 3
  fi
  echo "che372-d2: migration completed successfully within work_deadline (finished at $final_now_epoch, deadline $work_deadline_epoch)" >&2

  # LAST LINE OF DEFENSE #1 — final fencing check.
  #
  # The continuous-fencing samples above stop the moment the watchdog loop
  # breaks on the migrator's OS exit, but this script keeps doing real work
  # afterwards (the absence confirmation, this block). A foreign session
  # that appears in that window would never be seen by any sample, yet the
  # candidate would be admitted onto a database it was touching. Re-run the
  # gate once, right here, against the same verified-session registry.
  #
  # The migrator's own sessions are expected to be GONE by now (that is what
  # confirm_no_live_session_for_pid just proved), so this normally runs
  # against a fully empty database and the registry contributes nothing —
  # stale entries are dropped by quiescence.mjs's own re-verification rather
  # than fencing anything. Passing the registry anyway is deliberate: if a
  # migrator connection somehow outlived its process, it is still OURS and
  # should be reported as such by the absence check, not misattributed here.
  #
  # Failure routes to needs_operator, never to a silent success — consistent
  # with every other "we cannot prove it" path in this script.
  if ! run_final_gate "$work_deadline_monotonic_ms" "" "$migrator_verified_sessions"; then
    echo "che372-d2: final fencing check before admission did NOT pass — a session/lock/transaction unaccounted for by this attempt was present (or could not be observed) after the migrator exited; refusing to write starting_candidate" >&2
    write_decision "needs_operator_final_fencing_check_failed"
    exit 3
  fi

  # LAST LINE OF DEFENSE #2 — object validity.
  #
  # Exit zero from the migrator proves the process finished, not that the
  # objects it was supposed to create exist and are usable. The concrete
  # case (Astra's) is a concurrent index build that failed or was skipped by
  # IF NOT EXISTS: PostgreSQL leaves the index in the catalog with
  # indisvalid = false, the ledger records the migration as applied, and the
  # migrator exits zero. Admitting the candidate there puts it on a schema
  # whose indexes the planner will not use and whose unique constraints are
  # not enforced. See find_invalid_indexes for why this repo's own migration
  # rules make that outcome specifically easy to reach.
  if ! invalid_index_report="$(find_invalid_indexes "$work_deadline_monotonic_ms")"; then
    echo "che372-d2: post-migration object-validity check did not pass ($invalid_index_report) — an INVALID index (or an unobservable catalog) means exit zero does not prove the intended schema exists; refusing to write starting_candidate" >&2
    write_decision "needs_operator_invalid_index"
    exit 5
  fi

  # This is the exact "starting_candidate\n<attempt-id>" contract
  # docker/entrypoint.cd.sh polls for and freshness-checks against
  # CHE372_D2_ATTEMPT_ID before it will exec the server — the one point in
  # this script where the entrypoint's wait is actually released.
  write_decision "starting_candidate"
  exit 0
fi

if [ "$final_state" = "failed_nonzero" ]; then
  echo "che372-d2: migrator exited non-zero before deadline — state UNCERTAIN, compare full ledger/object state before any decision" >&2
  echo "che372-d2: confirming the exited migrator's Postgres session is actually gone before returning (a fast nonzero exit does not by itself prove the server-side session ended server-side, e.g. a crashed client can leave in-flight work) — routing through the same bounded session-confirmation path as deadline_exceeded, never skipped just because the exit was fast" >&2
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
echo "che372-d2: entering cancellation/confirmation path (${final_state}) for os_pid=$migrator_os_pid pg_pid=${migrator_pg_pid:-unknown}" >&2
if kill -0 "$migrator_os_pid" 2>/dev/null; then
  kill -TERM "$migrator_os_pid" 2>/dev/null || true
fi

cancel_deadline_epoch="$deadline_epoch"
cancel_deadline_monotonic_ms="$deadline_monotonic_ms"
os_confirmed=false
pg_confirmed=false
while [ "$(monotonic_ms)" -lt "$cancel_deadline_monotonic_ms" ]; do
  if ! $os_confirmed && ! kill -0 "$migrator_os_pid" 2>/dev/null; then
    wait "$migrator_os_pid" 2>/dev/null || true
    os_confirmed=true
  fi
  if [ -n "$migrator_pg_pid" ]; then
    if ! $pg_confirmed && confirm_no_live_session_for_pid "$migrator_pg_pid" "$migrator_pg_backend_start" "$cancel_deadline_monotonic_ms"; then
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
  sleep_clamped_to 100 "$cancel_deadline_monotonic_ms"
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
  while [ "$(monotonic_ms)" -lt "$deadline_monotonic_ms" ]; do
    kill -0 "$migrator_os_pid" 2>/dev/null || { os_confirmed=true; wait "$migrator_os_pid" 2>/dev/null || true; break; }
    sleep_clamped_to 100 "$deadline_monotonic_ms"
  done
fi

# confirm_no_live_session_for_pid checks its own remaining-time budget
# against deadline_monotonic_ms internally (see MIN_OBSERVER_BUDGET_MS
# above) and denies without spawning anything if too little time
# remains, so no separate outer clock check is needed here to keep this
# call from starting a doomed probe.
if [ -n "$migrator_pg_pid" ] && ! $pg_confirmed; then
  if confirm_no_live_session_for_pid "$migrator_pg_pid" "$migrator_pg_backend_start" "$deadline_monotonic_ms"; then
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
# failed_nonzero reaching here means the OS process already exited on its
# own (os_confirmed becomes true immediately in the loop above, since
# kill -0 already fails) and only the Postgres-session confirmation is
# actually in question. Its exit code is 1 (this script's existing
# defined contract for "migrator failed, non-timeout") ONLY when that
# session is actually confirmed absent; an UNCONFIRMED session after a
# nonzero exit is exactly as dangerous as an unconfirmed session after a
# deadline/cancellation — a crashed client can leave server-side work in
# flight — so it gets the same needs_operator/exit 3 fencing outcome, not
# a silent exit 1 that lets the fence come down on unverified state.
if $os_confirmed && $pg_confirmed; then
  if [ "$final_state" = "failed_nonzero" ]; then
    echo "che372-d2: migrator os_pid=$migrator_os_pid exited non-zero and its Postgres session pid=${migrator_pg_pid:-never-identified} is confirmed terminated — exit 1 (failed, not needs_operator)" >&2
    write_decision "failed_confirmed_terminated"
    exit 1
  fi
  echo "che372-d2: os_pid=$migrator_os_pid and Postgres pid=$migrator_pg_pid both confirmed terminated after ${final_state} — needs_operator (state uncertain: hooks or earlier statements may have committed)" >&2
  write_decision "needs_operator_confirmed_terminated_${final_state}"
else
  echo "che372-d2: termination UNCONFIRMED at or after the absolute deadline (os_confirmed=$os_confirmed pg_confirmed=$pg_confirmed pg_pid=${migrator_pg_pid:-never-identified}) — needs_operator, fence must stay up" >&2
  write_decision "needs_operator_unconfirmed_${final_state}"
fi
exit 3
