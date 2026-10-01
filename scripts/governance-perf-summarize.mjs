#!/usr/bin/env node
// Summarize BenchmarkGovernanceComment* raw samples (JSONL) into p50/p95 per
// mode and a PASS/FAIL/INCONCLUSIVE verdict (CHE-884 / CHE-866 B4).
//   node scripts/governance-perf-summarize.mjs <samples.jsonl>
// Acceptance: zero governance statements and provider calls when the flag is
// off; p95 added client time and DB time each <= max(1ms, 2% of the pooled
// bypassed p95). "Added" is the median over batches of the paired per-batch
// p95 delta (the benchmark interleaves both modes per request, so each batch is
// a matched pair): a host noise burst that hits a minority of batches cannot
// move it, a real per-request cost moves every batch. The pooled delta is
// reported for reference only. Needs >= 5 batches of >= 1000 samples per mode
// for both benches, else INCONCLUSIVE (empty input included). Unreadable input
// (missing file, bad JSONL line, a row with a missing or non-numeric field or an
// unknown Mode, rows from two runs merged into one file) exits 2. This defines "negligible" for review, not a production SLA.
import { readFileSync } from "node:fs";

const MIN_BATCHES = 5;
const MIN_PER_BATCH = 1000;
const file = process.argv[2];
if (!file) {
  console.error("usage: governance-perf-summarize.mjs <samples.jsonl>");
  process.exit(2);
}
const EXPECTED_BENCHES = ["create", "update"];
const usageError = (message) => {
  console.error(`governance-perf-summarize: ${message}`);
  process.exit(2);
};
let rows;
try {
  // Keep source line numbers: blank lines are skipped, not renumbered.
  rows = readFileSync(file, "utf8").split("\n").flatMap((l, i) => {
    if (!l) return [];
    try {
      return [{ row: JSON.parse(l), line: i + 1 }];
    } catch {
      return usageError(`${file}:${i + 1} is not valid JSON (truncated sample file?)`);
    }
  });
} catch (err) {
  usageError(`cannot read ${file}: ${err.message}`);
}
const MODES = ["bypassed", "flag_off_hook"];
// Counts and indexes are non-negative integers (a negative GovStmts would cancel a
// real one in the sum); timings are non-negative finite numbers.
const COUNTS = ["Batch", "Seq", "DBStmts", "GovStmts", "Provider"];
const TIMINGS = ["ClientNs", "DBNs"];
const invalidRow = (r) => {
  if (r === null || typeof r !== "object") return "not an object";
  if (typeof r.Bench !== "string") return "Bench is not a string";
  if (!MODES.includes(r.Mode)) return `unknown Mode ${JSON.stringify(r.Mode)}`;
  const count = COUNTS.find((k) => !Number.isInteger(r[k]) || r[k] < 0);
  if (count) return `${count} is not a non-negative integer`;
  const timing = TIMINGS.find((k) => !Number.isFinite(r[k]) || r[k] < 0);
  return timing && `${timing} is not a non-negative finite number`;
};
for (const { row, line } of rows) {
  const bad = invalidRow(row);
  if (bad) usageError(`${file}:${line} is invalid: ${bad}`);
}
rows = rows.map(({ row }) => row);
// The benchmark restarts batch numbering at 0 per process and appends to the file,
// so a second run into the same file repeats (Bench, Mode, Batch, Seq): reject it.
const seen = new Set();
for (const r of rows) {
  const key = `${r.Bench}/${r.Mode}/${r.Batch}/${r.Seq}`;
  if (seen.has(key)) usageError(`duplicate sample ${key}: use a fresh GOVERNANCE_PERF_SAMPLES file per run`);
  seen.add(key);
}

// Nearest-rank quantile: the smallest sample with at least q of the mass at or below it.
const quantile = (sorted, q) => sorted[Math.max(0, Math.ceil(q * sorted.length) - 1)];
const ms = (ns) => ns / 1e6;
// Upper median: the conservative middle for an even batch count.
const median = (xs) => [...xs].sort((a, b) => a - b)[Math.floor(xs.length / 2)];
const batchP95 = (set, col) => {
  const by = new Map();
  for (const r of set) (by.get(r.Batch) ?? by.set(r.Batch, []).get(r.Batch)).push(r[col]);
  return new Map([...by].map(([batch, v]) => [batch, quantile(v.sort((a, b) => a - b), 0.95)]));
};

let failed = false;
let inconclusive = false;
const report = {};
const benches = [...new Set([...EXPECTED_BENCHES, ...rows.map((r) => r.Bench)])];
for (const bench of benches) {
  const per = {};
  const sets = {};
  for (const mode of MODES) {
    const set = (sets[mode] = rows.filter((r) => r.Bench === bench && r.Mode === mode));
    const batches = new Map();
    for (const r of set) batches.set(r.Batch, (batches.get(r.Batch) ?? 0) + 1);
    const client = set.map((r) => r.ClientNs).sort((a, b) => a - b);
    const dbt = set.map((r) => r.DBNs).sort((a, b) => a - b);
    per[mode] = {
      n: set.length,
      batches: batches.size,
      shortBatches: [...batches.values()].filter((n) => n < MIN_PER_BATCH).length,
      clientP50Ms: ms(quantile(client, 0.5)),
      clientP95Ms: ms(quantile(client, 0.95)),
      dbP95Ms: ms(quantile(dbt, 0.95)),
      govStatements: set.reduce((a, r) => a + r.GovStmts, 0),
      providerCalls: set.reduce((a, r) => a + r.Provider, 0),
      dbStatementsPerRequestMean: set.length ? set.reduce((a, r) => a + r.DBStmts, 0) / set.length : 0,
      dbStatementsPerRequestMax: set.reduce((m, r) => Math.max(m, r.DBStmts), -Infinity),
      dbStatementsPerRequestMin: set.reduce((m, r) => Math.min(m, r.DBStmts), Infinity),
    };
  }
  const a = per.bypassed;
  const b = per.flag_off_hook;
  const pairedDeltas = (col) => {
    const base = batchP95(sets.bypassed, col);
    const hook = batchP95(sets.flag_off_hook, col);
    return [...base.keys()].filter((k) => hook.has(k)).sort((x, y) => x - y).map((k) => ms(hook.get(k) - base.get(k)));
  };
  const clientBatchAdded = pairedDeltas("ClientNs");
  const dbBatchAdded = pairedDeltas("DBNs");
  const clientAdded = median(clientBatchAdded);
  const dbAdded = median(dbBatchAdded);
  const clientLimit = Math.max(1, 0.02 * a.clientP95Ms);
  const dbLimit = Math.max(1, 0.02 * a.dbP95Ms);
  const checks = {
    zeroGovernanceStatements: b.govStatements === 0,
    zeroProviderCalls: b.providerCalls === 0,
    sameStatementCountAsBypassed:
      a.n === 0 || b.n === 0 || // a missing mode is INCONCLUSIVE (enough=false), not a statement mismatch
      (b.dbStatementsPerRequestMax === a.dbStatementsPerRequestMax &&
        b.dbStatementsPerRequestMin === a.dbStatementsPerRequestMin &&
        // min/max alone miss an extra statement on only some requests
        b.dbStatementsPerRequestMean <= a.dbStatementsPerRequestMean + 1e-9),
    clientP95WithinLimit: clientAdded <= clientLimit,
    dbP95WithinLimit: dbAdded <= dbLimit,
  };
  // Enough = >= MIN_BATCHES full batches per mode AND >= MIN_BATCHES matched pairs
  // (the median is over pairs). Missing data is inconclusive, never a timing FAIL.
  const enough =
    [a, b].every((m) => m.batches >= MIN_BATCHES && m.shortBatches === 0) && clientBatchAdded.length >= MIN_BATCHES;
  if (!enough) inconclusive = true;
  // Deterministic checks fail on any sample count; timing checks only with enough data.
  const { clientP95WithinLimit, dbP95WithinLimit, ...deterministic } = checks;
  if (!Object.values(deterministic).every(Boolean) || (enough && !(clientP95WithinLimit && dbP95WithinLimit))) failed = true;
  report[bench] = {
    per,
    clientAddedP95Ms: clientAdded, clientBatchAddedP95Ms: clientBatchAdded, pooledClientAddedP95Ms: b.clientP95Ms - a.clientP95Ms, clientLimitMs: clientLimit,
    dbAddedP95Ms: dbAdded, dbBatchAddedP95Ms: dbBatchAdded, pooledDbAddedP95Ms: b.dbP95Ms - a.dbP95Ms, dbLimitMs: dbLimit,
    checks, enoughSamples: enough,
  };
}
const verdict = failed ? "FAIL" : inconclusive ? "INCONCLUSIVE" : "PASS";
console.log(JSON.stringify({ verdict, report }, null, 2));
// exitCode, not exit(): lets a piped stdout flush the whole report first.
process.exitCode = failed ? 1 : inconclusive ? 3 : 0;
