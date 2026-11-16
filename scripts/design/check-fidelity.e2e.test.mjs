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
  const { r, result } = run("--case", "s24");
  assert.equal(r.status, 1);
  const v = result.violations.filter((x) => x.rule === "missing-render" && x.state === "s24");
  assert.equal(v.length, 1);
  assert.match(v[0].reason, /run-budget-top-up-issue-block/);
});

test("cases render their own state: s1, s2 and s4 give three different code renders", () => {
  const { result } = run("--case", "s1", "--case", "s2", "--case", "s4");
  const hash = (state) => result.renders.find((x) => x.state === state)?.code_root_sha256;
  for (const s of ["s1", "s2", "s4"]) assert.match(hash(s), /^[0-9a-f]{64}$/, `${s} has a code render hash`);
  assert.equal(new Set(["s1", "s2", "s4"].map(hash)).size, 3, "typed message and chosen Approve change the render");
  assert.deepEqual(result.renders.find((x) => x.state === "s2").steps, ["type [role=textbox]"]);
});

test("a state the harness cannot draw fails missing-render and is not drawn as another state", () => {
  const { result } = run("--case", "s3", "--case", "s1b");
  const reasons = (state) => result.violations.filter((x) => x.rule === "missing-render" && x.state === state).map((x) => x.reason);
  assert.match(reasons("s3").join("\n"), /run-budget-top-up-answered/, "s3 names its own fixture");
  assert.match(reasons("s1b").join("\n"), /step .*Details/, "s1b names the step the code cannot do");
});

// Every state the real card can draw has a harness fixture. s3 (in-card conversation), s24 (issue block) and s26 (DM
// grouped by issue) have no component yet: they fail missing-render, which is the gap, not a harness shortfall.
const DRAWABLE = ["s5", "s7", "s7b", "s8", "s9", "s11", "s12", "s13", "s14", "s15", "s16", "s17", "s18", "s19", "s20", "s21", "s23", "s25"];
test("every state the card can draw renders; each reaches its own state", () => {
  const { result } = run(...DRAWABLE.flatMap((s) => ["--case", s]));
  const failed = result.violations.filter((v) => v.rule === "missing-render").map((v) => `${v.state}: ${v.reason}`);
  assert.deepEqual(failed, [], "no drawable state fails to render");
  const hash = (s) => result.renders.find((x) => x.state === s).code_root_sha256;
  // same fixture family, different state => different render
  assert.notEqual(hash("s8"), hash("s9"), "approved vs rejected");
  assert.notEqual(hash("s14"), hash("s15"), "applied vs not applied");
  assert.notEqual(hash("s7"), hash("s7b"), "all chosen vs unchosen batch");
  assert.notEqual(hash("s12"), hash("s13"), "diff only vs Show full");
  assert.notEqual(hash("s20"), hash("s8"), "clicking the collapsed block opens the full card");
  // The code effect line shows the state the case names (read from the rule 6 instance, which quotes the code text).
  const codeText = (s, block) => result.violations.find((v) => v.state === s && v.rule === "text" && v.block === block)?.reason ?? "";
  assert.match(codeText("s8", "decision-request-effect"), /code "Applied\./);
  assert.match(codeText("s9", "decision-request-effect"), /code "Not applied\. You rejected it/);
  assert.match(codeText("s15", "decision-request-effect"), /code "Not applied/);
  assert.match(codeText("s17", "decision-request-effect"), /code "Expired\./);
  assert.match(codeText("s19", "decision-request-effect"), /code "Revoked\./);
});

test("s3, s24 and s26 have no component and fail missing-render", () => {
  const { result } = run("--case", "s3", "--case", "s24", "--case", "s26");
  for (const s of ["s3", "s24", "s26"]) {
    assert.ok(result.violations.some((v) => v.rule === "missing-render" && v.state === s), `${s} fails missing-render`);
  }
});
