import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const script = fileURLToPath(new URL("./governance-perf-summarize.mjs", import.meta.url));

// Both modes sit at ~8ms; the hook mode is 18ms for `slowShare` of the samples
// in each batch listed in `hookSlowBatches` (a host noise burst, not hook cost).
function summarize(t, { batches = 5, hookSlowBatches = [], slowShare = 0.15, govStmts = 0, provider = 0, hookStmts = 14, hookBatchOffset = 0, modes = ["bypassed", "flag_off_hook"] } = {}) {
  const n = 1000;
  const rows = [];
  for (let batch = 0; batch < batches; batch++) {
    for (const mode of modes) {
      for (let seq = 0; seq < n; seq++) {
        const slow = mode === "flag_off_hook" && hookSlowBatches.includes(batch) && seq < n * slowShare;
        const ns = slow ? 18e6 : 8e6 + (seq % 7) * 1e4;
        rows.push({
          Bench: "update", Mode: mode, Batch: mode === "flag_off_hook" ? batch + hookBatchOffset : batch, Seq: seq, ClientNs: ns, DBNs: ns - 1e5,
          DBStmts: mode === "flag_off_hook" ? hookStmts : 14,
          GovStmts: mode === "flag_off_hook" && batch === 0 && seq === 0 ? govStmts : 0,
          Provider: mode === "flag_off_hook" && batch === 0 && seq === 0 ? provider : 0,
        });
      }
    }
  }
  const dir = mkdtempSync(join(tmpdir(), "gov-perf-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const file = join(dir, "samples.jsonl");
  writeFileSync(file, rows.map((r) => JSON.stringify(r)).join("\n"));
  const run = spawnSync(process.execPath, [script, file], { encoding: "utf8" });
  assert.ok(run.stdout, `summarizer produced no output (exit ${run.status}): ${run.stderr}`);
  return { status: run.status, out: JSON.parse(run.stdout) };
}

test("noise in a minority of batches does not flip the verdict", (t) => {
  // Pooled over all batches the hook mode is 6% slow, so its pooled p95 is 18ms;
  // the median per-batch paired delta is ~0 because 3 of 5 batches are clean.
  const { status, out } = summarize(t, { hookSlowBatches: [0, 1] });
  assert.equal(out.verdict, "PASS");
  assert.equal(status, 0);
  assert.ok(out.report.update.pooledClientAddedP95Ms > 1, "pooled delta is the old, noise-following gate");
  assert.equal(out.report.update.clientBatchAddedP95Ms.length, 5);
});

test("a hook slower in most batches fails the gate", (t) => {
  const { status, out } = summarize(t, { hookSlowBatches: [0, 1, 2] });
  assert.equal(out.verdict, "FAIL");
  assert.equal(out.report.update.checks.clientP95WithinLimit, false);
  assert.equal(out.report.update.checks.dbP95WithinLimit, false);
  assert.equal(status, 1);
});

test("a governance statement with the flag off fails regardless of timing", (t) => {
  const { status, out } = summarize(t, { govStmts: 1 });
  assert.equal(out.verdict, "FAIL");
  assert.equal(out.report.update.checks.zeroGovernanceStatements, false);
  assert.equal(status, 1);
});

test("fewer than five batches is inconclusive, never a pass", (t) => {
  const { status, out } = summarize(t, { batches: 4 });
  assert.equal(out.verdict, "INCONCLUSIVE");
  assert.equal(status, 3);
});

test("a provider call with the flag off fails", (t) => {
  const { status, out } = summarize(t, { provider: 1 });
  assert.equal(out.report.update.checks.zeroProviderCalls, false);
  assert.equal(status, 1);
});

test("a statement-count mismatch fails", (t) => {
  const { status, out } = summarize(t, { hookStmts: 15 });
  assert.equal(out.report.update.checks.sameStatementCountAsBypassed, false);
  assert.equal(status, 1);
});

test("a missing mode is inconclusive, not a timing FAIL", (t) => {
  const { status, out } = summarize(t, { modes: ["bypassed"] });
  assert.equal(out.verdict, "INCONCLUSIVE");
  assert.equal(status, 3);
});

test("disjoint batch ids leave no matched pairs: inconclusive", (t) => {
  const { status, out } = summarize(t, { hookBatchOffset: 100 });
  assert.equal(out.verdict, "INCONCLUSIVE");
  assert.equal(status, 3);
});

test("noisy data under five batches is inconclusive, not FAIL", (t) => {
  const { status, out } = summarize(t, { batches: 3, hookSlowBatches: [0, 1, 2] });
  assert.equal(out.verdict, "INCONCLUSIVE");
  assert.equal(status, 3);
});
