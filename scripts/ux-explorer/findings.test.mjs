import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { ROUTES, PROFILES, validateFinding, selectWeekly, WEEKLY_CAP } from "./findings.mjs";

// CHE-2985 (B2 of CHE-2970): the explorer run definition and finding format.
const skill = readFileSync(new URL("../../skills/ux-explorer/SKILL.md", import.meta.url), "utf8");

const shot = (profile) => ({ profile, file: `before-${profile}.png` });
const option = (n) => ({ title: `option ${n}`, critique: `critique ${n}` });
const class2 = (over = {}) => ({
  class: 2, form_factor: "desktop", title: "t", problem: "p", pain: 5,
  screenshots: [shot("desktop")], options: [option(1), option(2)], recommendation: 1, ...over,
});
const class1 = (over = {}) => ({
  class: 1, form_factor: "phone", title: "t", steps: ["open /issues"],
  measurement: { metric: "scroll_delta_px", value: 120, profile: "phone" }, ...over,
});

test("fixed list of 10 sandbox screens, visited on desktop and phone", () => {
  assert.equal(ROUTES.length, 10);
  assert.equal(new Set(ROUTES.map((r) => r.id)).size, 10);
  assert.deepEqual(Object.keys(PROFILES).sort(), ["desktop", "phone"]);
  for (const r of ROUTES) assert.ok(skill.includes(r.path), `SKILL.md lists ${r.path}`);
});

test("SKILL.md fixes tools, identities and duplicate search", () => {
  for (const s of ["/qa-only", "/design-review", "multica issue search", "identity E", "identity P", "ux-explorer-sandbox"]) {
    assert.ok(skill.includes(s), s);
  }
  assert.match(skill, /E's credential is never available to the publication step/);
});

test("class 1 needs steps, a measurement and a form factor", () => {
  assert.deepEqual(validateFinding(class1()), []);
  assert.ok(validateFinding(class1({ steps: [] })).length);
  assert.ok(validateFinding(class1({ measurement: undefined })).length);
  assert.ok(validateFinding(class1({ form_factor: "tablet" })).length);
});

test("class 2 needs a before screenshot per affected form factor", () => {
  assert.deepEqual(validateFinding(class2()), []);
  assert.deepEqual(validateFinding(class2({ form_factor: "both", screenshots: [shot("desktop"), shot("phone")] })), []);
  assert.ok(validateFinding(class2({ form_factor: "both" })).length, "phone screenshot missing");
});

test("class 2 needs 2-3 options with critiques, a recommendation and a pain score", () => {
  assert.ok(validateFinding(class2({ options: [option(1)] })).length);
  assert.ok(validateFinding(class2({ options: [option(1), option(2), option(3), option(4)] })).length);
  assert.ok(validateFinding(class2({ options: [option(1), { title: "x" }] })).length);
  assert.ok(validateFinding(class2({ recommendation: 5 })).length);
  assert.ok(validateFinding(class2({ pain: 0 })).length);
  assert.ok(validateFinding(class2({ pain: 11 })).length);
  assert.ok(validateFinding(class2({ problem: "" })).length);
});

test("max 20 class 2 proposals per week, ranked by pain, extras carry over", () => {
  assert.equal(WEEKLY_CAP, 20);
  const pending = Array.from({ length: 25 }, (_, i) => ({ id: i, pain: (i % 10) + 1, found_at: `2026-11-${10 + i}` }));
  const { publish, carryOver } = selectWeekly(pending);
  assert.equal(publish.length, 20);
  assert.equal(carryOver.length, 5);
  assert.ok(publish.every((p, i) => i === 0 || publish[i - 1].pain >= p.pain), "ranked high to low");
  assert.ok(Math.max(...carryOver.map((p) => p.pain)) <= Math.min(...publish.map((p) => p.pain)));
  assert.deepEqual(selectWeekly(pending.slice(0, 3)).carryOver, []);
});

test("equal pain: older proposal first", () => {
  const { publish } = selectWeekly([
    { id: "new", pain: 5, found_at: "2026-11-09" },
    { id: "old", pain: 5, found_at: "2026-11-02" },
  ]);
  assert.deepEqual(publish.map((p) => p.id), ["old", "new"]);
});
