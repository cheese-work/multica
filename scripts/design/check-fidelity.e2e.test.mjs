// Renders the real decision card and the lock with headless Chrome. Needs a built harness (vite) and Playwright
// Chromium, so it is not in the fast frontend-quality list; the ui-fidelity job runs it (CHE-5793 step 8).
//   node --test scripts/design/check-fidelity.e2e.test.mjs
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

const run = (...args) => {
  const out = mkdtempSync(join(tmpdir(), "fidelity-"));
  const r = spawnSync("node", ["scripts/design/check-fidelity.mjs", "--out", out, ...args], { encoding: "utf8" });
  const file = join(out, "result.json");
  return { r, out, result: existsSync(file) ? JSON.parse(readFileSync(file, "utf8")) : null };
};
const has = (result, rule, block) => result.violations.some((v) => v.rule === rule && v.block === block && v.state === "s1");

test("decision card s1: the 5 known gaps each fail with their named reason", () => {
  const { r, out, result } = run("--case", "s1");
  assert.equal(r.status, 1, r.stderr);
  assert.ok(result, "result.json is written");
  assert.ok(has(result, "text", "decision-request-asker"), "title: the header text differs from the lock");
  assert.ok(has(result, "missing-block", "decision-request-title"), "title block");
  assert.ok(has(result, "missing-block", "decision-request-meta"), "4-row before/after table");
  assert.ok(has(result, "missing-block", "decision-request-effect"), "'If you do nothing' line");
  assert.ok(has(result, "missing-block", "decision-request-choice"), "full-width Approve/Reject");
  assert.ok(has(result, "missing-block", "decision-request-details"), "Details");
  assert.ok(existsSync(join(out, "side-by-side-s1-card-540.png")), "side-by-side PNG");
});

test("result JSON lists one instance per rule x block x case; rule 6 carries the code text hash", () => {
  const { result } = run("--case", "s1");
  const keys = result.violations.map((v) => `${v.rule}|${v.block}|${v.state}|${v.profile}`);
  assert.equal(new Set(keys).size, keys.length, "no duplicate instance");
  for (const v of result.violations.filter((x) => x.rule === "text")) assert.match(v.code_text_sha256, /^[0-9a-f]{64}$/);
});

test("a case whose fixture the harness does not have fails missing-render, naming the fixture", () => {
  const { r, result } = run("--case", "s7");
  assert.equal(r.status, 1);
  const v = result.violations.filter((x) => x.rule === "missing-render" && x.state === "s7");
  assert.equal(v.length, 1);
  assert.match(v[0].reason, /batch-run-budget/);
});
