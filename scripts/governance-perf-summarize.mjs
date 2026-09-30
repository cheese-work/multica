#!/usr/bin/env node
// Summarize BenchmarkGovernanceComment* raw samples (JSONL) into p50/p95 per
// mode and a PASS/FAIL/INCONCLUSIVE verdict (CHE-884 / CHE-866 B4).
//   node scripts/governance-perf-summarize.mjs <samples.jsonl>
// Acceptance: zero governance statements and provider calls when the flag is
// off; p95 added client time and DB time each <= max(1ms, 2% of the matched
// bypassed-hook p95). "Added" is the median over batches of the paired per-batch
// p95 delta (the benchmark interleaves both modes per request, so each batch is
// a matched pair): a host noise burst that hits a minority of batches cannot
// move it, a real per-request cost moves every batch. The pooled delta is
// reported for reference only. Needs >= 5 batches of >= 1000 samples per mode,
// else INCONCLUSIVE. This defines "negligible" for review, not a production SLA.
import { readFileSync } from "node:fs";

const MIN_BATCHES = 5;
const MIN_PER_BATCH = 1000;
const file = process.argv[2];
if (!file) {
  console.error("usage: governance-perf-summarize.mjs <samples.jsonl>");
  process.exit(2);
}
const rows = readFileSync(file, "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l));

// Nearest-rank quantile: the smallest sample with at least q of the mass at or below it.
const quantile = (sorted, q) => sorted[Math.max(0, Math.ceil(q * sorted.length) - 1)];
const ms = (ns) => ns / 1e6;
// Upper median: the conservative middle for an even batch count.
const median = (xs) => [...xs].sort((a, b) => a - b)[Math.floor(xs.length / 2)];
const batchP95 = (set, col) => {
  const by = new Map();
  for (const r of set) by.set(r.Batch, [...(by.get(r.Batch) ?? []), r[col]]);
  return new Map([...by].map(([batch, v]) => [batch, quantile(v.sort((a, b) => a - b), 0.95)]));
};

let failed = false;
let inconclusive = false;
const report = {};
for (const bench of [...new Set(rows.map((r) => r.Bench))]) {
  const per = {};
  const sets = {};
  for (const mode of ["bypassed", "flag_off_hook"]) {
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
      dbStatementsPerRequestMax: Math.max(...set.map((r) => r.DBStmts)),
      dbStatementsPerRequestMin: Math.min(...set.map((r) => r.DBStmts)),
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
      b.dbStatementsPerRequestMax === a.dbStatementsPerRequestMax &&
      b.dbStatementsPerRequestMin === a.dbStatementsPerRequestMin,
    clientP95WithinLimit: clientAdded <= clientLimit,
    dbP95WithinLimit: dbAdded <= dbLimit,
  };
  const enough = [a, b].every((m) => m.batches >= MIN_BATCHES && m.shortBatches === 0);
  if (!enough) inconclusive = true;
  if (!Object.values(checks).every(Boolean)) failed = true;
  report[bench] = {
    per,
    clientAddedP95Ms: clientAdded, clientBatchAddedP95Ms: clientBatchAdded, pooledClientAddedP95Ms: b.clientP95Ms - a.clientP95Ms, clientLimitMs: clientLimit,
    dbAddedP95Ms: dbAdded, dbBatchAddedP95Ms: dbBatchAdded, pooledDbAddedP95Ms: b.dbP95Ms - a.dbP95Ms, dbLimitMs: dbLimit,
    checks, enoughSamples: enough,
  };
}
const verdict = failed ? "FAIL" : inconclusive ? "INCONCLUSIVE" : "PASS";
console.log(JSON.stringify({ verdict, report }, null, 2));
process.exit(failed ? 1 : inconclusive ? 3 : 0);
