#!/usr/bin/env node
//
// D2 quiescence controller: passive preflight + final admission gate for the
// migration-safety cutover controller (CHE-372). See deploy/cd/README.md.
//
// This module never mutates the target database. It only runs short,
// autocommit, metadata-only observation queries against pg_stat_activity,
// pg_locks, pg_prepared_xacts, and pg_stat_progress_create_index, then
// applies the fail-closed admission rules from the approved design.
//
// Connection: every observation query runs through a fresh `psql` invocation
// (autocommit by construction — psql issues one statement per invocation
// here, no BEGIN) so no long-lived snapshot or idle-in-transaction session is
// ever held by the observer itself. Each invocation carries its own
// statement_timeout via PGOPTIONS and an outer wall-clock timeout via the
// `timeout` coreutil, so a hung server-side query cannot stall the caller
// past --query-timeout-ms.
//
// Exit codes: 0 = admitted. 1 = denied (see JSON `reason`/`evidence`). 2 =
// usage error. Denial is always the fail-closed default: any query failure,
// missing field, short read, or ambiguous state is treated as NOT quiescent.

import { spawnSync } from "node:child_process";

export const YOUNG_TRANSACTION_THRESHOLD_MS = 15000;
// DEFAULT_QUERY_TIMEOUT_MS is the design's <=500ms ceiling for the actual
// SQL statement (server-side statement_timeout/lock_timeout/
// idle_in_transaction_session_timeout, applied in runPsql below) — it is
// a policy input from the approved D2 addendum, not a knob to widen for
// convenience.
//
// DEFAULT_WALL_CLOCK_TIMEOUT_MS is a SEPARATE, CLI-only default for the
// outer `timeout` wrapper around the whole observer invocation, used only
// when a caller does not supply --query-timeout-ms explicitly (e.g. an
// interactive/manual preflight or final-gate check run outside
// migrate-supervised.sh's own precise deadline arithmetic). It must
// additionally cover real process-spawn overhead beyond the SQL statement
// itself — --psql-via-docker-network mode's `docker run --rm
// postgres:16-alpine psql ...` measured consistently around 450ms of
// client+container overhead on X99 with a warm image cache, independent
// of query complexity, so a wall-clock ceiling equal to the 500ms
// statement budget alone was frequently and flakily too tight. Callers
// that know their own precise remaining time budget (migrate-supervised.sh)
// pass --query-timeout-ms explicitly and this default is never consulted.
export const DEFAULT_QUERY_TIMEOUT_MS = 500;
// 2000ms: measured --psql-via-docker-network overhead is ~450-550ms
// under light load, but the same host running several observation calls
// in close succession (as this repo's own test suites do, deliberately,
// to exercise concurrent contention scenarios) pushed that past 1000ms
// under real measurement -- Docker daemon API latency is not a fixed
// constant. This default only governs callers that never supply their
// own --query-timeout-ms; migrate-supervised.sh always does, computed
// from its own precise remaining-time budget, so this generous default
// never weakens that deadline-critical path.
export const DEFAULT_WALL_CLOCK_TIMEOUT_MS = 2000;
export const DEFAULT_LOCK_TIMEOUT_MS = 1000;

function fail(message) {
  throw new Error(message);
}

function option(name, args, { required = true, fallback } = {}) {
  const index = args.indexOf(name);
  if (index === -1 || args[index + 1] === undefined) {
    if (required && fallback === undefined) fail(`missing ${name}`);
    return fallback;
  }
  return args[index + 1];
}

// runPsql executes exactly one SQL statement over a fresh psql process:
// autocommit, tuples-only, one row per line, fields pipe-separated, NULL
// rendered as the literal token <NULL> so it is distinguishable from an
// empty string. The whole invocation is wall-clock bounded by
// queryTimeoutMs via the `timeout` coreutil (SIGKILL escalation) in
// addition to the server-side statement_timeout, so a psql process that
// itself hangs (e.g. on connection) cannot outlive the caller's budget.
//
// Returns { ok: true, rows } on success, or { ok: false, reason } on any
// failure — including timeout, non-zero exit, or malformed output. This
// function never throws for a server/connection failure; the caller
// interprets ok:false as "unknown state," which the fail-closed rules
// above always resolve to denial.
export function runPsql({ connInfo, sql, queryTimeoutMs = DEFAULT_WALL_CLOCK_TIMEOUT_MS }) {
  const statementTimeoutMs = Math.max(1, Math.min(queryTimeoutMs, 500));
  const pgoptions = `-c statement_timeout=${statementTimeoutMs} -c lock_timeout=${statementTimeoutMs} -c idle_in_transaction_session_timeout=${statementTimeoutMs}`;
  // The outer wall-clock bound must never exceed what the caller actually
  // asked for. A previous version computed
  // `Math.ceil((queryTimeoutMs + 500) / 1000)` here, which always padded
  // by up to 500ms AND rounded up to a whole second — so a caller passing
  // even a very small queryTimeoutMs (deliberately, because its own
  // remaining time budget was small) still got a full 1-second `timeout`
  // wrapper, silently letting this call run far longer than the caller's
  // actual remaining time. GNU `timeout` accepts fractional seconds
  // directly, so this now passes the caller's real budget with no padding
  // and no rounding up — only a floor to guarantee `timeout` receives a
  // positive, non-zero duration.
  const wallClockTimeoutMs = Math.max(1, queryTimeoutMs);
  const wallClockTimeoutSeconds = wallClockTimeoutMs / 1000;
  const fieldSep = "\x01";
  const recordSep = "\x02";
  const appName = connInfo.applicationName ?? "che372-d2-quiescence-observer";

  // --dbname accepts a full libpq connection URI, so the target is always
  // explicit on the command line rather than depending on an environment
  // variable psql may or may not read (DATABASE_URL is this repo's
  // convention, not a libpq-recognized one).
  const psqlFlags = [
    "--no-psqlrc",
    "--quiet",
    "--tuples-only",
    "--no-align",
    `--field-separator=${fieldSep}`,
    `--record-separator=${recordSep}`,
    "--set=ON_ERROR_STOP=1",
    `--dbname=${connInfo.databaseUrl}`,
    "--command",
    sql,
  ];

  const runEnv = {
    PGOPTIONS: pgoptions,
    PGCONNECT_TIMEOUT: String(Math.max(1, Math.ceil(queryTimeoutMs / 1000))),
    PGAPPNAME: appName,
  };

  // connInfo.command lets the caller substitute how `psql` itself is
  // invoked (e.g. inside a short-lived Docker container attached to the
  // isolated test network) without this function knowing about Docker.
  // The default runs `psql` directly off PATH. Either way the returned
  // prefix's last element must resolve to the `psql` entrypoint; this
  // function always appends psqlFlags itself.
  const commandPrefix = connInfo.command ? connInfo.command(runEnv) : ["psql"];
  // -s KILL: send SIGKILL immediately at the deadline rather than
  // `timeout`'s default SIGTERM. This matters specifically for the
  // --psql-via-docker-network path, where the wrapped command is `docker
  // run --rm ... psql`: SIGTERM lets the `docker` CLI attempt a graceful
  // container stop/detach with the daemon, which was observed taking over
  // 2 seconds in practice against an unreachable target — more than
  // 40x the requested budget. SIGKILL ends the `docker` client process
  // immediately; this observer never needs graceful shutdown semantics,
  // only a hard ceiling on its own wall-clock footprint.
  const fullArgs = ["-s", "KILL", `${wallClockTimeoutSeconds}s`, ...commandPrefix, ...psqlFlags];

  const result = spawnSync("timeout", fullArgs, {
    encoding: "utf8",
    env: {
      ...process.env,
      ...(connInfo.command ? {} : runEnv),
      ...connInfo.env,
    },
  });

  if (result.error) {
    return { ok: false, reason: `psql spawn failed: ${result.error.message}` };
  }
  // `timeout -s KILL` (see above) can report completion either as exit
  // code 124/137 (the traditional `timeout` convention) OR, depending on
  // how the signal propagated through the wrapped process (notably
  // `docker run`, whose own client process may itself be the one that
  // receives and forwards SIGKILL), as spawnSync's status:null with
  // signal:'SIGKILL' — Node reports a process terminated directly by an
  // uncaught signal this way, never falling back to a synthesized exit
  // code. Both must be treated identically: a real wall-clock timeout,
  // not a generic "unexpected null status" failure.
  if (result.status === 124 || result.status === 137 || result.signal === "SIGKILL" || result.signal === "SIGTERM") {
    return { ok: false, reason: "observation query exceeded wall-clock timeout" };
  }
  if (result.status !== 0) {
    return { ok: false, reason: `observation query failed (exit ${result.status}${result.signal ? `, signal ${result.signal}` : ""}): ${(result.stderr || "").trim()}` };
  }

  const raw = result.stdout.endsWith(recordSep) ? result.stdout.slice(0, -recordSep.length) : result.stdout;
  // psql's --no-align terminates every record with its own newline
  // regardless of --record-separator, so each record's last field carries
  // a trailing "\n" that is not part of the SQL value. Strip exactly one
  // trailing newline per record (never trim broader whitespace, which
  // could silently eat meaningful trailing spaces in a query/text field).
  const rows = raw.length === 0
    ? []
    : raw.split(recordSep).filter((r) => r.length > 0)
        .map((record) => (record.endsWith("\n") ? record.slice(0, -1) : record))
        .map((record) => record.split(fieldSep));
  return { ok: true, rows };
}

// nowMs is injectable so tests can control sample-age arithmetic without
// wall-clock sleeps.
export function observeQuiescenceState({ connInfo, queryTimeoutMs = DEFAULT_WALL_CLOCK_TIMEOUT_MS, now = () => Date.now() }) {
  const observedAtMs = now();

  // One combined query per observation to bound total query count and
  // therefore total time; each sub-query gets tagged with a `kind` column
  // so the caller can distinguish sources without needing five round
  // trips. `application_name` lets us exclude only this observer's own
  // statements (never a broader class of "controller" connections).
  const sql = `
    SELECT 'activity' AS kind,
      pid::text, backend_start::text, datname, usename, backend_type,
      COALESCE(xact_start::text, '<NULL>') AS xact_start,
      state, COALESCE(backend_xid::text, '<NULL>') AS backend_xid,
      COALESCE(backend_xmin::text, '<NULL>') AS backend_xmin,
      COALESCE(wait_event_type, '<NULL>') AS wait_event_type,
      COALESCE(wait_event, '<NULL>') AS wait_event,
      COALESCE(query, '') AS query,
      application_name
    FROM pg_stat_activity
    WHERE datname = current_database()
      AND pid <> pg_backend_pid()
    UNION ALL
    SELECT 'lock', l.pid::text, '<NULL>', d.datname, '<NULL>', '<NULL>',
      '<NULL>', l.mode, '<NULL>', '<NULL>', '<NULL>', '<NULL>',
      l.locktype || ':' || COALESCE(l.relation::text, '') || ':' || l.granted::text,
      '<NULL>'
    FROM pg_locks l
    JOIN pg_database d ON d.oid = l.database
    WHERE d.datname = current_database() AND l.pid <> pg_backend_pid()
    UNION ALL
    SELECT 'prepared', '<NULL>', '<NULL>', database, gid, '<NULL>',
      transaction::text, '<NULL>', '<NULL>', '<NULL>', '<NULL>', '<NULL>',
      '<NULL>', '<NULL>'
    FROM pg_prepared_xacts
    WHERE database = current_database()
    UNION ALL
    SELECT 'create_index', p.pid::text, '<NULL>', d.datname, '<NULL>', '<NULL>',
      '<NULL>', p.phase, '<NULL>', '<NULL>', '<NULL>', '<NULL>',
      COALESCE(p.current_locker_pid::text, ''), '<NULL>'
    FROM pg_stat_progress_create_index p
    JOIN pg_database d ON d.oid = p.datid
    WHERE d.datname = current_database();
  `;

  const result = runPsql({ connInfo, sql, queryTimeoutMs });
  const observationEndMs = now();
  if (!result.ok) {
    return { ok: false, reason: result.reason, observedAtMs, observationEndMs };
  }

  const activity = [];
  const locks = [];
  const prepared = [];
  const createIndex = [];
  for (const row of result.rows) {
    const kind = row[0];
    if (kind === "activity") {
      activity.push({
        pid: row[1], backendStart: row[2], datname: row[3], usename: row[4],
        backendType: row[5], xactStart: row[6] === "<NULL>" ? null : row[6],
        state: row[7], backendXid: row[8] === "<NULL>" ? null : row[8],
        backendXmin: row[9] === "<NULL>" ? null : row[9],
        waitEventType: row[10] === "<NULL>" ? null : row[10],
        waitEvent: row[11] === "<NULL>" ? null : row[11],
        query: row[12], applicationName: row[13],
      });
    } else if (kind === "lock") {
      locks.push({ pid: row[1], datname: row[3], mode: row[7], detail: row[12] });
    } else if (kind === "prepared") {
      prepared.push({ datname: row[3], gid: row[4], transaction: row[6] });
    } else if (kind === "create_index") {
      createIndex.push({ pid: row[1], phase: row[7], currentLockerPid: row[12] });
    }
  }

  return { ok: true, observedAtMs, observationEndMs, activity, locks, prepared, createIndex };
}

// Self-exclusion is handled entirely server-side by the observation SQL's
// `pid <> pg_backend_pid()` clause (observeQuiescenceState above): a
// session can never see or filter out its own current backend PID by
// claiming a name, because pg_backend_pid() is assigned by the server at
// connection time and is not client-settable.
//
// A previous version of this module additionally excluded any row whose
// client-controlled application_name matched a fixed observer prefix.
// That was a real admission hole: application_name is a session GUC any
// client can set to an arbitrary string (`PGAPPNAME`, `SET
// application_name`, or a connection-string parameter), so a foreign
// session could claim the same prefix and make its own open transaction
// or retained snapshot invisible to the final gate. It was also
// unnecessary — runPsql uses child_process.spawnSync, which blocks until
// the single psql process it launches exits, so no two observation
// connections from this module are ever open at the same time; each
// query's own connection is excluded from its own results purely by
// being the query's own backend, which pg_backend_pid() already covers.
// Do not reintroduce a name/label-based exclusion here; if a future
// caller genuinely needs to exclude an additional session, it must do so
// by an unforgeable identity (recorded PID + backend_start, as
// migrate-supervised.sh's watchdog already requires for termination),
// never by a string the excluded session controls.

// evaluatePassivePreflight implements the design's gate 1: a coarse, cheap
// check performed while the healthy release still serves. It never denies
// because of a young transaction (that only means "keep going"); it denies
// on anything already known to be a hard blocker so the caller does not
// start an outage to wait out a problem it can already see.
export function evaluatePassivePreflight(state, opts = {}) {
  const thresholdMs = opts.youngTransactionThresholdMs ?? YOUNG_TRANSACTION_THRESHOLD_MS;

  if (!state.ok) {
    return { admit: false, decision: "deferred_contention", reason: `observation failed: ${state.reason}`, evidence: {} };
  }

  // state.activity already excludes this observation's own connection via
  // the query's server-side `pid <> pg_backend_pid()` clause — an
  // unforgeable exclusion. No client-controlled field (application_name
  // or otherwise) is used to hide any row here.
  const foreignActivity = state.activity;

  const oldTransactions = foreignActivity.filter((row) => {
    if (!row.xactStart) return false;
    const ageMs = state.observedAtMs - Date.parse(row.xactStart);
    return Number.isFinite(ageMs) && ageMs >= thresholdMs;
  });
  if (oldTransactions.length > 0) {
    return {
      admit: false, decision: "deferred_contention",
      reason: "at least one transaction is already at or past the 15000ms age threshold",
      evidence: { oldTransactions },
    };
  }

  if (state.prepared.length > 0) {
    return { admit: false, decision: "deferred_contention", reason: "prepared transaction present", evidence: { prepared: state.prepared } };
  }

  const advisoryLockHolders = state.locks.filter((l) => l.detail.startsWith("advisory:"));
  if (advisoryLockHolders.length > 0) {
    return { admit: false, decision: "deferred_contention", reason: "a foreign advisory-lock holder is present", evidence: { advisoryLockHolders } };
  }

  const unknownBackendTypes = foreignActivity.filter((row) =>
    row.backendType !== "client backend" && row.backendType !== "background worker" &&
    row.backendType !== "walsender" && row.backendType !== "autovacuum worker",
  );
  if (unknownBackendTypes.length > 0) {
    return { admit: false, decision: "deferred_contention", reason: "unknown/unfenceable consumer backend type present", evidence: { unknownBackendTypes } };
  }

  if (state.createIndex.length > 0) {
    return { admit: false, decision: "deferred_contention", reason: "a concurrent index build is already in progress", evidence: { createIndex: state.createIndex } };
  }

  return { admit: true, decision: "preflight_admitted", reason: "no known blockers; young transactions if any may attempt to quiesce", evidence: { foreignActivityCount: foreignActivity.length } };
}

// evaluateFinalGate implements the design's gate 2: the strict, zero-
// tolerance check run immediately before the first candidate mutation and
// re-run immediately before migrator launch. Unknown state (a failed
// observation) always denies; it is never treated as "empty set."
export function evaluateFinalGate(state, opts = {}) {
  if (!state.ok) {
    return { admit: false, decision: "final_gate_denied", reason: `observation failed (unknown state fails closed): ${state.reason}`, evidence: {} };
  }

  // See evaluatePassivePreflight: self-exclusion is server-side and
  // unforgeable (pg_backend_pid()); no client-controlled field is used
  // to exclude any row here.
  const foreignActivity = state.activity;

  // opts.fencedPids names the pid allowlist of sessions the caller has
  // already independently confirmed (e.g. the migrator's own verified
  // pg_backend_pid) — every check below must apply this exemption
  // consistently, not just the lock checks. Otherwise a fenced pid's own
  // legitimate in-flight transaction/prepared-xact/index-build/advisory
  // lock would deny the gate even though the caller has already vouched
  // for exactly that pid, which defeats continuous fencing (final-gate
  // re-checked with --fenced-sessions <pid>|<backend_start> while that pid
  // is still running). Any row attributable to a pid NOT in the allowlist
  // still denies.
  //
  // This function does not and cannot judge WHETHER a pid deserves to be
  // fenced — that is the caller's burden, and cliFinalGate discharges it by
  // re-verifying each claimed (pid, backend_start) pair against
  // pg_stat_activity before it ever reaches this allowlist. Never populate
  // this set from a role/database match alone: "currently authenticated as
  // role R" is not ownership, and a set built that way exempts every
  // foreign client holding that role's credentials from every check below.
  const fencedPids = new Set((opts.fencedPids ?? []).map(String));

  // Zero non-operation transactions/retained snapshots: any foreign
  // session with an open xact_start, OR any foreign session holding a
  // backend_xmin/backend_xid (a retained snapshot even without an open
  // xact_start, e.g. a REPEATABLE READ/SERIALIZABLE read-only
  // transaction or a long-running cursor), denies.
  const openTransactionsOrSnapshots = foreignActivity.filter((row) =>
    !fencedPids.has(String(row.pid)) &&
    (row.xactStart !== null || row.backendXid !== null || row.backendXmin !== null),
  );
  if (openTransactionsOrSnapshots.length > 0) {
    return {
      admit: false, decision: "final_gate_denied",
      reason: "non-operation transaction or retained snapshot present",
      evidence: { openTransactionsOrSnapshots },
    };
  }

  const unfencedPrepared = state.prepared.filter((p) => !fencedPids.has(String(p.pid)));
  if (unfencedPrepared.length > 0) {
    return { admit: false, decision: "final_gate_denied", reason: "prepared transaction present", evidence: { prepared: unfencedPrepared } };
  }

  const advisoryLockHolders = state.locks.filter((l) => l.detail.startsWith("advisory:") && !fencedPids.has(String(l.pid)));
  if (advisoryLockHolders.length > 0) {
    return { admit: false, decision: "final_gate_denied", reason: "a foreign advisory-lock holder is present", evidence: { advisoryLockHolders } };
  }

  // No unaccounted conflicting session-level locks/index ops. Idle
  // sessions with no open transaction/snapshot are OK only if fenced.
  // Any lock held by a pid not in that allowlist denies.
  const unaccountedLocks = state.locks.filter((l) => !fencedPids.has(String(l.pid)));
  if (unaccountedLocks.length > 0) {
    return {
      admit: false, decision: "final_gate_denied",
      reason: "unaccounted conflicting session-level lock present",
      evidence: { unaccountedLocks },
    };
  }

  const unfencedCreateIndex = state.createIndex.filter((c) => !fencedPids.has(String(c.pid)));
  if (unfencedCreateIndex.length > 0) {
    return { admit: false, decision: "final_gate_denied", reason: "a concurrent index build is in progress", evidence: { createIndex: unfencedCreateIndex } };
  }

  // Any remaining foreign backend at all — even idle, with no
  // transaction/snapshot/lock — must be in the fenced allowlist. This is
  // the "idle sessions OK only if fenced" rule; an unfenced idle session
  // is an unknown/unaccounted consumer and denies.
  const unfencedIdle = foreignActivity.filter((row) => !fencedPids.has(String(row.pid)));
  if (unfencedIdle.length > 0) {
    return {
      admit: false, decision: "final_gate_denied",
      reason: "unfenced foreign session present (idle sessions require explicit fencing)",
      evidence: { unfencedIdle },
    };
  }

  return { admit: true, decision: "final_gate_admitted", reason: "zero non-operation transactions, snapshots, prepared xacts, foreign locks, or unfenced sessions", evidence: {} };
}

// SampleTracker enforces the sampling/coverage rule during quiesce/migration:
// samples every <=250ms, and two consecutive missing samples OR a sample
// older than 1000ms is a hard failure. It never substitutes an assumed-
// healthy value for a missing sample.
export class SampleTracker {
  constructor({ intervalMs = 250, maxSampleAgeMs = 1000 } = {}) {
    this.intervalMs = intervalMs;
    this.maxSampleAgeMs = maxSampleAgeMs;
    this.consecutiveMisses = 0;
    this.lastSampleAtMs = null;
    this.samples = [];
  }

  record(state, nowMs) {
    if (!state.ok) {
      this.consecutiveMisses += 1;
      this.samples.push({ ok: false, atMs: nowMs, reason: state.reason });
      if (this.consecutiveMisses >= 2) {
        return { coverageOk: false, reason: "two consecutive missing samples" };
      }
      return { coverageOk: true, reason: null };
    }
    if (this.lastSampleAtMs !== null) {
      const age = nowMs - this.lastSampleAtMs;
      if (age > this.maxSampleAgeMs) {
        this.consecutiveMisses += 1;
        this.samples.push({ ok: false, atMs: nowMs, reason: `sample age ${age}ms exceeds ${this.maxSampleAgeMs}ms` });
        return { coverageOk: false, reason: `sample age ${age}ms exceeds ${this.maxSampleAgeMs}ms` };
      }
    }
    this.consecutiveMisses = 0;
    this.lastSampleAtMs = nowMs;
    this.samples.push({ ok: true, atMs: nowMs });
    return { coverageOk: true, reason: null };
  }

  coverageReport() {
    return {
      totalSamples: this.samples.length,
      missedSamples: this.samples.filter((s) => !s.ok).length,
      consecutiveMisses: this.consecutiveMisses,
    };
  }
}

// A role/database-scoped default (ALTER ROLE ... IN DATABASE ... SET) was
// tried here previously and removed. It looked unbypassable because
// PostgreSQL applies it to every new connection from that role/database —
// true, but only as a DEFAULT: a client-supplied connection-string
// "options=" parameter (or PGOPTIONS) is applied by pgx AFTER role/database
// defaults and therefore overrides it completely (verified against pgx
// v5's pgconn/config.go precedence and PostgreSQL's own GUC precedence
// rules), so a malicious or merely misconfigured DATABASE_URL could
// silently disable the "enforced" timeout. It also persisted past the
// migrator's own process exit, since it is server-side database metadata,
// not a client-session setting — leaking into unrelated later connections
// from the same role/database until explicitly reset.
//
// Timeout enforcement now lives inside the migrator's own Go process:
// server/internal/dbstartup.NewPoolWithEnforcedTimeouts sets and reads
// back statement_timeout/lock_timeout via an AfterConnect hook that runs
// strictly after pgx has finished applying the connection string, on
// every physical connection the pool opens — the pinned advisory-lock
// connection and every hook connection alike — so it is the LAST setting
// applied and cannot be overridden by anything in the connection string.
// See deploy/cd/migrate-supervised.sh, which sets
// MULTICA_INTERNAL_D2_ENFORCED_STATEMENT_TIMEOUT_MS/
// MULTICA_INTERNAL_D2_ENFORCED_LOCK_TIMEOUT_MS in the migrator's
// environment. This module intentionally does not attempt to verify that
// enforcement from outside — PostgreSQL provides no view for reading back
// another live backend's session-level GUC value at all, so any such
// probe would necessarily inspect the wrong connection.

// verifyLiveSessionHoldsAttemptLock confirms that the specific pid holds a
// GRANTED shared advisory lock at attemptLockKey — a fresh,
// cryptographically random secret the CD supervisor generates once per
// migration attempt and passes to the migrator ONLY via
// MULTICA_INTERNAL_D2_ATTEMPT_LOCK_KEY (see server/cmd/migrate/main.go and
// dbstartup.NewPoolWithEnforcedTimeouts's AfterConnect hook, which acquires
// it on every connection the migrator's pool opens, main and hook
// connections alike). The key is never logged, never written to any file,
// never part of a connection string, and never sent to this observer's own
// connection — so a foreign client, which by construction cannot know this
// run's secret, cannot acquire the same key and cannot be mistaken for a
// session this attempt actually opened.
//
// This is the load-bearing difference from role/database/pid/backend_start
// alone: those only prove "authenticated as the migration role right now",
// which a foreign client sharing that role's credentials can trivially also
// be. Holding a secret this observer only knows because the SAME shell
// invocation that launched the migrator also generated it is proof the
// session traces back to this exact attempt.
//
// Shared lock mode (not exclusive) is required because the migrator opens
// more than one connection over a run (its pinned migration connection plus
// hook connections); all of them must be able to hold this marker
// concurrently without blocking each other.
export function verifyLiveSessionHoldsAttemptLock({ connInfo, pid, attemptLockKey, queryTimeoutMs = DEFAULT_WALL_CLOCK_TIMEOUT_MS }) {
  const sql = `
    SELECT count(*)::text
    FROM pg_locks
    WHERE locktype = 'advisory'
      AND objsubid = 1
      AND classid = ${attemptLockClassid(attemptLockKey)}
      AND objid = ${attemptLockObjid(attemptLockKey)}
      AND mode = 'ShareLock'
      AND granted = true
      AND pid = ${Number(pid)};
  `;
  const result = runPsql({ connInfo, sql, queryTimeoutMs });
  if (!result.ok) {
    return { ok: false, reason: `could not read pg_locks for pid ${pid}: ${result.reason}` };
  }
  if (result.rows.length !== 1) {
    return { ok: false, reason: `expected exactly one count row, found ${result.rows.length} (unknown state fails closed)` };
  }
  const count = Number(result.rows[0][0]);
  if (count !== 1) {
    return { ok: false, reason: `pid ${pid} does not currently hold this attempt's advisory-lock marker (found ${count} matching grants, expected 1) — not attempt-bound, cannot be treated as ours` };
  }
  return { ok: true };
}

// attemptLockClassid/attemptLockObjid split a bigint advisory-lock key into
// the two int4 halves PostgreSQL stores in pg_locks.classid/objid for the
// single-key pg_advisory_lock*(bigint) form (LOCKTAG_ADVISORY encoding).
function attemptLockClassid(attemptLockKey) {
  return Number(BigInt.asIntN(32, BigInt(attemptLockKey) >> 32n));
}
function attemptLockObjid(attemptLockKey) {
  return Number(BigInt.asIntN(32, BigInt(attemptLockKey) & 0xffffffffn));
}

// verifyLiveSessionIsCovered confirms that a specific, already-connected
// backend (identified by the unforgeable pid + backend_start pair the
// watchdog itself recorded when it observed the session — never by a
// client-supplied application_name or label) is running as the expected
// role/database AND holds this attempt's secret advisory-lock marker, so
// this exact recorded connection is attempt-bound to THIS migration run
// and not merely "some session authenticated as the migration role" —
// which a foreign client sharing that role's credentials could also be.
// Role/database/pid/backend_start alone proves identity continuity (no PID
// reuse); the attempt-lock check proves this connection was opened by THIS
// attempt's own migrator process, since only it was ever told the secret
// key. attemptLockKey is required — there is no "skip the ownership proof"
// mode, since that is exactly the admission hole this function exists to
// close.
export function verifyLiveSessionIsCovered({ connInfo, pid, backendStart, expectedRoleName, expectedDatabaseName, attemptLockKey, queryTimeoutMs = DEFAULT_WALL_CLOCK_TIMEOUT_MS }) {
  if (!attemptLockKey) {
    return { ok: false, reason: "no attemptLockKey supplied — cannot prove attempt-bound ownership, unknown state fails closed" };
  }
  const sql = `
    SELECT usename, datname, backend_start::text
    FROM pg_stat_activity
    WHERE pid = ${Number(pid)};
  `;
  const result = runPsql({ connInfo, sql, queryTimeoutMs });
  if (!result.ok) {
    return { ok: false, reason: `could not read pg_stat_activity for pid ${pid}: ${result.reason}` };
  }
  if (result.rows.length !== 1) {
    return { ok: false, reason: `expected exactly one live session for pid ${pid}, found ${result.rows.length} (unknown state fails closed)` };
  }
  const [usename, datname, actualBackendStart] = result.rows[0];
  if (actualBackendStart !== backendStart) {
    return { ok: false, reason: `pid ${pid} backend_start mismatch (expected ${backendStart}, observed ${actualBackendStart}) — PID likely reused by a different session` };
  }
  if (usename !== expectedRoleName || datname !== expectedDatabaseName) {
    return { ok: false, reason: `pid ${pid} is role=${usename} db=${datname}, expected role=${expectedRoleName} db=${expectedDatabaseName}` };
  }
  const lockCheck = verifyLiveSessionHoldsAttemptLock({ connInfo, pid, attemptLockKey, queryTimeoutMs });
  if (!lockCheck.ok) {
    return { ok: false, reason: `role/database/backend_start match but attempt-bound ownership check failed: ${lockCheck.reason}` };
  }
  return { ok: true };
}

// findLiveSessionByRole locates the oldest live session authenticated as
// the given role/database, excluding this observation's own connection
// (server-side, via pg_backend_pid() — see the note on runPsql above).
// It is used to discover the migrator's actual Postgres backend PID and
// backend_start after launch, so the caller can then pass that
// unforgeable identity to verifyLiveSessionIsCovered rather than
// confusing an OS-level child process ID (which is not a Postgres
// backend PID) with the number pg_stat_activity actually reports.
//
// This finds ONE session — the first one to discover the migrator's
// identity when nothing has been recorded yet. It is not the right tool
// for checking whether a SPECIFIC, already-recorded session has ended:
// with more than one same-role/database session live (the migrator's own
// hook connections, or an unrelated session), LIMIT 1 can return a
// different PID than the one the caller is asking about, wrongly implying
// the recorded one is gone. Use findLiveSessionsForRole (plural) plus an
// exact pid+backend_start membership check for that — see
// verifyLiveSessionIsAbsent below, which is what the D2 watchdog's
// termination-confirmation path must use instead.
export function findLiveSessionByRole({ connInfo, roleName, databaseName, queryTimeoutMs = DEFAULT_WALL_CLOCK_TIMEOUT_MS }) {
  const sql = `
    SELECT pid::text, backend_start::text
    FROM pg_stat_activity
    WHERE usename = '${roleName.replace(/'/g, "''")}'
      AND datname = '${databaseName.replace(/'/g, "''")}'
      AND pid <> pg_backend_pid()
    ORDER BY backend_start ASC
    LIMIT 1;
  `;
  const result = runPsql({ connInfo, sql, queryTimeoutMs });
  if (!result.ok) {
    return { ok: false, reason: `could not search pg_stat_activity: ${result.reason}` };
  }
  if (result.rows.length !== 1) {
    return { ok: false, reason: `no live session found for role=${roleName} db=${databaseName} (unknown state fails closed)` };
  }
  const [pid, backendStart] = result.rows[0];
  return { ok: true, pid, backendStart };
}

// findLiveSessionsForRole returns every live session (pid + backend_start)
// authenticated as the given role/database, excluding this observation's
// own connection. Used to account for ALL of the migrator's attributable
// connections during termination confirmation — the pinned advisory-lock
// connection plus any separately-opened hook connections its pool may
// still hold — not just the single PID first discovered at launch.
//
// This establishes "who is currently authenticated as this role", nothing
// more. It is a discovery/accounting primitive, NOT an ownership proof: its
// output must never be fed directly into a fence (evaluateFinalGate's
// fencedPids), because that would exempt any foreign client holding the
// same role credentials from every quiescence check. Legitimate use is the
// conservative direction only — treating an extra same-role session as a
// reason to DENY confirmation (verifyLiveSessionIsAbsent), or as a
// CANDIDATE to be independently verified via verifyLiveSessionIsCovered
// before the caller records it as its own.
export function findLiveSessionsForRole({ connInfo, roleName, databaseName, queryTimeoutMs = DEFAULT_WALL_CLOCK_TIMEOUT_MS }) {
  const sql = `
    SELECT pid::text, backend_start::text
    FROM pg_stat_activity
    WHERE usename = '${roleName.replace(/'/g, "''")}'
      AND datname = '${databaseName.replace(/'/g, "''")}'
      AND pid <> pg_backend_pid();
  `;
  const result = runPsql({ connInfo, sql, queryTimeoutMs });
  if (!result.ok) {
    return { ok: false, reason: `could not search pg_stat_activity: ${result.reason}` };
  }
  return { ok: true, sessions: result.rows.map(([pid, backendStart]) => ({ pid, backendStart })) };
}

// verifyLiveSessionIsAbsent proves — or fails to prove — that a SPECIFIC,
// previously-recorded session (identified by its unforgeable pid +
// backend_start, never by a fresh unrelated query result) is no longer
// live, while also accounting for every other session still authenticated
// as the same role/database (migrator-attributable hook connections, or
// an ambiguous same-role session this script must not silently ignore).
//
// This is the fail-closed contract the D2 watchdog's cancellation
// confirmation requires: an observation FAILURE (query error, connection
// refused, timeout) is UNKNOWN state, not "absent" — it must never be
// treated as proof of termination. A caller must check `result.ok` before
// trusting `result.absent`; there is no default that makes ok:false imply
// anything about absence.
export function verifyLiveSessionIsAbsent({ connInfo, pid, backendStart, roleName, databaseName, queryTimeoutMs = DEFAULT_WALL_CLOCK_TIMEOUT_MS }) {
  const found = findLiveSessionsForRole({ connInfo, roleName, databaseName, queryTimeoutMs });
  if (!found.ok) {
    return { ok: false, reason: `could not confirm absence (observation failed, unknown state fails closed): ${found.reason}` };
  }
  const recordedStillLive = found.sessions.some((s) => s.pid === String(pid) && s.backendStart === backendStart);
  const otherSessions = found.sessions.filter((s) => !(s.pid === String(pid) && s.backendStart === backendStart));
  if (recordedStillLive) {
    return { ok: true, absent: false, reason: `recorded session pid=${pid} backend_start=${backendStart} is still live`, otherSessions };
  }
  if (otherSessions.length > 0) {
    return {
      ok: true, absent: false,
      reason: `recorded session pid=${pid} is gone, but ${otherSessions.length} other role=${roleName}/db=${databaseName} session(s) remain — cannot confirm all migrator-attributable connections have ended`,
      otherSessions,
    };
  }
  return { ok: true, absent: true, otherSessions: [] };
}

// findInvalidIndexes reports every index in the target database left
// INVALID (pg_index.indisvalid = false). This is a post-migration OBJECT
// VALIDITY check, not a quiescence check, but it belongs in this module
// because it needs exactly the same bounded, autocommit, metadata-only
// observation path and must never mutate anything.
//
// A failed CREATE INDEX CONCURRENTLY leaves its index in the catalog with
// indisvalid = false: present and named, but ignored by the planner and
// not enforcing a unique constraint. Nothing about the migrator's exit code
// reveals this — and this repo's own migration rules (concurrent builds
// mandatory, conditionally-skipped migrations still recorded in
// schema_migrations, later migrations therefore using IF NOT EXISTS) make
// it specifically likely to pass unnoticed: the IF NOT EXISTS retry sees
// the invalid index as already present and skips. Excludes system catalogs
// so only application-visible objects are reported.
//
// Fail-closed like everything else here: an observation failure returns
// ok:false with a reason, never an empty list.
export function findInvalidIndexes({ connInfo, queryTimeoutMs = DEFAULT_WALL_CLOCK_TIMEOUT_MS }) {
  const sql = `
    SELECT c.relname::text
    FROM pg_index i
    JOIN pg_class c ON c.oid = i.indexrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE i.indisvalid = false
      AND n.nspname NOT IN ('pg_catalog', 'pg_toast', 'information_schema');
  `;
  const result = runPsql({ connInfo, sql, queryTimeoutMs });
  if (!result.ok) {
    return { ok: false, reason: `could not read pg_index (unknown state fails closed): ${result.reason}` };
  }
  return { ok: true, invalidIndexes: result.rows.map((row) => row[0]) };
}

// buildConnInfo resolves how observation queries reach Postgres.
//
// Default: invoke the `psql` binary directly on PATH with DATABASE_URL.
// This is what a CD runner host with postgresql-client installed uses.
//
// --psql-via-docker-network <network>: this host's `psql` client is not
// guaranteed to be present (it is not part of this repo's runtime image),
// but Docker always is on the X99 CD runners (see docker/entrypoint.sh /
// deploy/cd/qualification.sh, which already assume Docker). In this mode
// every observation query runs inside a short-lived, immediately-removed
// `postgres:16-alpine` container attached to the given Docker network, so
// no extra host dependency is required and the mechanism matches D1's
// existing synthetic-fixture convention.
function buildConnInfo(databaseUrl, opts = {}) {
  if (opts.dockerNetwork) {
    return {
      databaseUrl,
      command: (extraEnv) => {
        const envArgs = Object.entries(extraEnv).flatMap(([k, v]) => ["--env", `${k}=${v}`]);
        return [
          "docker", "run", "--rm", "--network", opts.dockerNetwork,
          ...envArgs,
          "postgres:16-alpine",
          "psql",
        ];
      },
      env: {},
    };
  }
  return { databaseUrl, command: null, env: {} };
}

function connOptsFromArgs(args) {
  const databaseUrl = option("--database-url", args, { required: false, fallback: process.env.DATABASE_URL }) || fail("missing --database-url");
  const dockerNetwork = option("--psql-via-docker-network", args, { required: false, fallback: undefined });
  const queryTimeoutMs = Number(option("--query-timeout-ms", args, { required: false, fallback: String(DEFAULT_QUERY_TIMEOUT_MS) }));
  return { databaseUrl, dockerNetwork, queryTimeoutMs };
}

function cliPreflight(args) {
  const { databaseUrl, dockerNetwork, queryTimeoutMs } = connOptsFromArgs(args);
  const state = observeQuiescenceState({ connInfo: buildConnInfo(databaseUrl, { dockerNetwork }), queryTimeoutMs });
  const result = evaluatePassivePreflight(state);
  process.stdout.write(`${JSON.stringify(result)}\n`);
  process.exitCode = result.admit ? 0 : 1;
}

// FENCED_SESSION_FIELD_SEPARATOR separates a pid from its backend_start
// inside one --fenced-sessions entry. It must NOT be ":" — Postgres
// renders backend_start as e.g. "2026-09-14 10:30:00.123456+00", which
// already contains colons, so a ":"-separated pair could not be split
// unambiguously. "," is already this file's established separator BETWEEN
// multi-value entries (see --fenced-pids), so it is kept for that role
// and "|" is used within an entry. Neither character can appear in a pid
// (digits only) or in a Postgres timestamptz rendering.
const FENCED_SESSION_FIELD_SEPARATOR = "|";
const FENCED_SESSION_ENTRY_SEPARATOR = ",";

function cliFinalGate(args) {
  const { databaseUrl, dockerNetwork, queryTimeoutMs } = connOptsFromArgs(args);
  // --fenced-pids is the RAW, unverified pid allowlist. It exists for the
  // one call site where verification is structurally impossible: the
  // pre-launch gate, when the migrator has not started and owns no live
  // session yet. It is never used to exempt a session that is already
  // live, because at that point an unforgeable identity IS available and
  // must be demanded instead (--fenced-sessions below).
  const fencedPidsRaw = option("--fenced-pids", args, { required: false, fallback: "" });
  const fencedPids = fencedPidsRaw ? fencedPidsRaw.split(FENCED_SESSION_ENTRY_SEPARATOR).filter(Boolean) : [];
  const fencedSessionsRaw = option("--fenced-sessions", args, { required: false, fallback: "" });
  const roleName = option("--role-name", args, { required: false, fallback: "" });
  const databaseName = option("--database-name", args, { required: false, fallback: "" });
  const attemptLockKey = option("--attempt-lock-key", args, { required: false, fallback: "" });
  const connInfo = buildConnInfo(databaseUrl, { dockerNetwork });

  // A single previously-discovered pid is not enough to fence a live
  // migrator: the same OS process/role can legitimately hold more than one
  // Postgres backend at once (its main migration connection plus a
  // separately-opened metadata/hook connection), and those extra
  // connections come and go over the run.
  //
  // A previous version of this CLI solved that with
  // --fenced-role/--fenced-database, which re-resolved every live session
  // for that role at gate time and fenced all of them. That was a real
  // admission hole, and precisely the one the design principle documented
  // above (see the note before evaluatePassivePreflight) forbids: "who is
  // currently authenticated as role R on database D" is not ownership. Any
  // foreign client holding that role's credentials — an operator's psql, a
  // stray application instance, an attacker with the migration role — was
  // exempted from EVERY quiescence check and could transact undetected
  // through the whole migration window. The backend_start of each resolved
  // session was discarded outright, so not even PID reuse was guarded.
  //
  // --fenced-sessions replaces it with an explicit, caller-maintained
  // registry of sessions the caller has ALREADY discovered and verified for
  // itself, each named by the unforgeable (pid, backend_start) pair.
  // Membership is never re-derived here from role/database alone. Each
  // listed pair is re-verified RIGHT NOW, against this exact moment's
  // pg_stat_activity, via the same verifyLiveSessionIsCovered primitive the
  // watchdog uses — so a pair that has since been recycled onto a different
  // session (PID reuse), or that never matched the expected role/database,
  // is not fenced no matter what the caller claims.
  //
  // Failure handling follows this file's "fail closed on ambiguity, but a
  // confirmed-gone session is not ambiguous" philosophy (cf.
  // verifyLiveSessionIsAbsent): a pair that no longer verifies is simply
  // dropped from the allowlist rather than failing the whole gate. Dropping
  // it is the CONSERVATIVE action — an unfenced pid can only ever cause a
  // denial, never an admission, so a stale entry costs the caller nothing
  // but honesty. If that pid is in fact still holding state under a
  // different identity, it is now unfenced and the gate denies, which is
  // exactly right. What must never happen is the inverse: trusting an
  // unverified claim.
  const fencedSessionEntries = fencedSessionsRaw
    ? fencedSessionsRaw.split(FENCED_SESSION_ENTRY_SEPARATOR).filter(Boolean)
    : [];
  const droppedSessions = [];
  if (fencedSessionEntries.length > 0) {
    if (!roleName || !databaseName || !attemptLockKey) {
      process.stdout.write(`${JSON.stringify({ admit: false, decision: "final_gate_denied", reason: "--fenced-sessions requires --role-name, --database-name, and --attempt-lock-key so each claimed session can be verified against an expected identity and proven attempt-bound (unknown state fails closed)", evidence: {} })}\n`);
      process.exitCode = 1;
      return;
    }
    for (const entry of fencedSessionEntries) {
      const separatorIndex = entry.indexOf(FENCED_SESSION_FIELD_SEPARATOR);
      if (separatorIndex === -1) {
        droppedSessions.push({ entry, reason: `malformed entry (expected pid${FENCED_SESSION_FIELD_SEPARATOR}backend_start)` });
        continue;
      }
      const pid = entry.slice(0, separatorIndex);
      const backendStart = entry.slice(separatorIndex + FENCED_SESSION_FIELD_SEPARATOR.length);
      if (!/^\d+$/.test(pid) || backendStart.length === 0) {
        droppedSessions.push({ entry, reason: "malformed entry (pid must be numeric and backend_start non-empty)" });
        continue;
      }
      const covered = verifyLiveSessionIsCovered({
        connInfo, pid, backendStart,
        expectedRoleName: roleName, expectedDatabaseName: databaseName, attemptLockKey, queryTimeoutMs,
      });
      if (!covered.ok) {
        droppedSessions.push({ entry, reason: covered.reason });
        continue;
      }
      fencedPids.push(pid);
    }
  }

  const state = observeQuiescenceState({ connInfo, queryTimeoutMs });
  const result = evaluateFinalGate(state, { fencedPids });
  if (droppedSessions.length > 0) {
    result.evidence = { ...result.evidence, droppedFencedSessions: droppedSessions };
  }
  process.stdout.write(`${JSON.stringify(result)}\n`);
  process.exitCode = result.admit ? 0 : 1;
}

function cliFindLiveSessionByRole(args) {
  const { databaseUrl, dockerNetwork, queryTimeoutMs } = connOptsFromArgs(args);
  const roleName = option("--role-name", args);
  const databaseName = option("--database-name", args);
  const result = findLiveSessionByRole({ connInfo: buildConnInfo(databaseUrl, { dockerNetwork }), roleName, databaseName, queryTimeoutMs });
  process.stdout.write(`${JSON.stringify(result)}\n`);
  process.exitCode = result.ok ? 0 : 1;
}

// cliFindLiveSessionsForRole exposes findLiveSessionsForRole (plural) so a
// shell caller can enumerate CANDIDATE same-role sessions. Its output is
// explicitly not an ownership claim about any of them — see the function's
// own doc comment. The only correct use of this listing is to feed each
// candidate through verify-live-session-is-covered before recording it;
// handing this output straight to final-gate as a fence is the exact
// admission hole --fenced-role/--fenced-database was removed for.
function cliFindLiveSessionsForRole(args) {
  const { databaseUrl, dockerNetwork, queryTimeoutMs } = connOptsFromArgs(args);
  const roleName = option("--role-name", args);
  const databaseName = option("--database-name", args);
  const result = findLiveSessionsForRole({ connInfo: buildConnInfo(databaseUrl, { dockerNetwork }), roleName, databaseName, queryTimeoutMs });
  process.stdout.write(`${JSON.stringify(result)}\n`);
  process.exitCode = result.ok ? 0 : 1;
}

// cliListInvalidIndexes exit codes: 0 = the check ran and found none; 1 =
// the check ran and found at least one, OR the observation failed. Both
// non-zero cases are "do not proceed" for the caller; the JSON on stdout
// distinguishes them for logging.
function cliListInvalidIndexes(args) {
  const { databaseUrl, dockerNetwork, queryTimeoutMs } = connOptsFromArgs(args);
  const result = findInvalidIndexes({ connInfo: buildConnInfo(databaseUrl, { dockerNetwork }), queryTimeoutMs });
  process.stdout.write(`${JSON.stringify(result)}\n`);
  process.exitCode = result.ok && result.invalidIndexes.length === 0 ? 0 : 1;
}

function cliVerifyLiveSessionIsCovered(args) {
  const { databaseUrl, dockerNetwork, queryTimeoutMs } = connOptsFromArgs(args);
  const pid = option("--pid", args);
  const backendStart = option("--backend-start", args);
  const expectedRoleName = option("--role-name", args);
  const expectedDatabaseName = option("--database-name", args);
  const attemptLockKey = option("--attempt-lock-key", args);
  const result = verifyLiveSessionIsCovered({
    connInfo: buildConnInfo(databaseUrl, { dockerNetwork }), pid, backendStart,
    expectedRoleName, expectedDatabaseName, attemptLockKey, queryTimeoutMs,
  });
  process.stdout.write(`${JSON.stringify(result)}\n`);
  process.exitCode = result.ok ? 0 : 1;
}

// cliVerifyLiveSessionIsAbsent exit codes distinguish three states a
// shell caller must never conflate: 0 = confirmed absent (both the
// recorded session and every other same-role/database session are gone),
// 1 = confirmed NOT absent (the recorded session or another
// migrator-attributable session is still live — a known, real negative),
// 2 = observation failed (unknown state; the caller must treat this the
// same as "not absent," i.e. fail closed, never as success).
function cliVerifyLiveSessionIsAbsent(args) {
  const { databaseUrl, dockerNetwork, queryTimeoutMs } = connOptsFromArgs(args);
  const pid = option("--pid", args);
  const backendStart = option("--backend-start", args);
  const roleName = option("--role-name", args);
  const databaseName = option("--database-name", args);
  const result = verifyLiveSessionIsAbsent({
    connInfo: buildConnInfo(databaseUrl, { dockerNetwork }), pid, backendStart,
    roleName, databaseName, queryTimeoutMs,
  });
  process.stdout.write(`${JSON.stringify(result)}\n`);
  if (!result.ok) {
    process.exitCode = 2;
  } else {
    process.exitCode = result.absent ? 0 : 1;
  }
}

const isMain = process.argv[1] && import.meta.url === `file://${process.argv[1]}`;
if (isMain) {
  try {
    const [command, ...args] = process.argv.slice(2);
    if (command === "preflight") cliPreflight(args);
    else if (command === "final-gate") cliFinalGate(args);
    else if (command === "find-live-session-by-role") cliFindLiveSessionByRole(args);
    else if (command === "find-live-sessions-for-role") cliFindLiveSessionsForRole(args);
    else if (command === "verify-live-session-is-covered") cliVerifyLiveSessionIsCovered(args);
    else if (command === "verify-live-session-is-absent") cliVerifyLiveSessionIsAbsent(args);
    else if (command === "list-invalid-indexes") cliListInvalidIndexes(args);
    else fail("usage: quiescence.mjs <preflight|final-gate|find-live-session-by-role|find-live-sessions-for-role|verify-live-session-is-covered|verify-live-session-is-absent|list-invalid-indexes> --database-url postgres://... [--query-timeout-ms N] [--fenced-pids p1,p2] [--fenced-sessions 'pid|backend_start,pid|backend_start' --role-name R --database-name D --attempt-lock-key K] [--role-name R --database-name D --attempt-lock-key K [--pid P --backend-start TS]]");
  } catch (error) {
    process.stderr.write(`quiescence: ${error.message}\n`);
    process.exitCode = 2;
  }
}
