#!/usr/bin/env node
// Summarize BenchmarkGovernanceComment* raw samples (JSONL) into p50/p95 per
// mode and a PASS/FAIL/INCONCLUSIVE verdict (CHE-884 / CHE-866 B4).
//   node scripts/governance-perf-summarize.mjs <samples.jsonl>
// Acceptance: zero governance statements and provider calls when the flag is
// off; p95 added client time and DB time each <= max(1ms, 2% of the matched
// bypassed-hook p95). Needs >= 5 batches of >= 1000 samples per mode, else
// INCONCLUSIVE. This defines "negligible" for review, not a production SLA.
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

let failed = false;
let inconclusive = false;
const report = {};
for (const bench of [...new Set(rows.map((r) => r.Bench))]) {
  const per = {};
  for (const mode of ["bypassed", "flag_off_hook"]) {
    const set = rows.filter((r) => r.Bench === bench && r.Mode === mode);
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
  const clientAdded = b.clientP95Ms - a.clientP95Ms;
  const dbAdded = b.dbP95Ms - a.dbP95Ms;
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
  report[bench] = { per, clientAddedP95Ms: clientAdded, clientLimitMs: clientLimit, dbAddedP95Ms: dbAdded, dbLimitMs: dbLimit, checks, enoughSamples: enough };
}
const verdict = failed ? "FAIL" : inconclusive ? "INCONCLUSIVE" : "PASS";
console.log(JSON.stringify({ verdict, report }, null, 2));
process.exit(failed ? 1 : inconclusive ? 3 : 0);
