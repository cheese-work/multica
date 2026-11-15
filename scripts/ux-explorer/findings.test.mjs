import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, readFileSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import {
  ROUTES, PROFILES, validateFinding, selectWeekly, WEEKLY_CAP,
  allowRequest, isolationProblems, checkScreenshotFiles, PAT_PATH, SANDBOX_ORIGIN_ENV,
} from "./findings.mjs";

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

// Review round 1 (CHE-2985, terra): enforced boundaries, not prose.

test("explorer browser may only read: GET/HEAD to the sandbox origin", () => {
  const o = "https://app.example";
  assert.equal(allowRequest("GET", `${o}/ux-explorer-sandbox/issues`, o), true);
  assert.equal(allowRequest("HEAD", `${o}/x`, o), true);
  for (const m of ["POST", "PUT", "PATCH", "DELETE"]) assert.equal(allowRequest(m, `${o}/api/issues`, o), false, m);
  assert.equal(allowRequest("GET", "https://evil.example/x", o), false, "other origin");
  assert.equal(allowRequest("GET", "not a url", o), false, "unparseable fails closed");
});

test("publication step: refuses when E's credential is reachable", () => {
  const clean = { env: { PATH: "/usr/bin" }, canRead: () => false };
  assert.deepEqual(isolationProblems(clean), []);
  assert.ok(isolationProblems({ ...clean, canRead: (p) => p === PAT_PATH }).length, "PAT file readable");
  assert.ok(isolationProblems({ ...clean, env: { MULTICA_TOKEN: "x" } }).length, "token in env");
  assert.ok(isolationProblems({ ...clean, env: { EXPLORER_PAT: "x" } }).length, "explorer pat in env");
  assert.ok(isolationProblems({ ...clean, env: { [SANDBOX_ORIGIN_ENV]: "x" } }).length === 0, "origin is not a secret");
});

test("screenshot file names: plain .png name inside findings/, nothing else", () => {
  for (const file of ["/etc/passwd", "../x.png", "a/b.png", ".hidden.png", "x.jpg", "x.png\0.txt", ""]) {
    assert.ok(validateFinding(class2({ screenshots: [{ profile: "desktop", file }] })).length, JSON.stringify(file));
  }
});

test("screenshot files: regular PNG only, no symlink, no other content", () => {
  const dir = mkdtempSync(join(tmpdir(), "uxs-"));
  const png = Buffer.concat([Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]), Buffer.alloc(8)]);
  writeFileSync(join(dir, "ok.png"), png);
  writeFileSync(join(dir, "text.png"), "secret=1\n");
  writeFileSync(join(dir, "target.txt"), "x");
  symlinkSync(join(dir, "target.txt"), join(dir, "link.png"));
  mkdirSync(join(dir, "dir.png"));
  const f = (file) => class2({ screenshots: [{ profile: "desktop", file }] });
  assert.deepEqual(checkScreenshotFiles(dir, f("ok.png")), []);
  for (const bad of ["text.png", "link.png", "dir.png", "missing.png"]) {
    assert.ok(checkScreenshotFiles(dir, f(bad)).length, bad);
  }
});

test("weekly cap counts proposals already published this week", () => {
  const pending = Array.from({ length: 30 }, (_, i) => ({ id: i, pain: 5, found_at: `2026-11-${10 + i}` }));
  assert.equal(selectWeekly(pending, WEEKLY_CAP, 15).publish.length, 5);
  assert.equal(selectWeekly(pending, WEEKLY_CAP, 15).carryOver.length, 25);
  assert.equal(selectWeekly(pending, WEEKLY_CAP, 20).publish.length, 0);
  assert.equal(selectWeekly(pending, WEEKLY_CAP, 99).publish.length, 0, "over cap never goes negative");
  assert.equal(selectWeekly(pending, WEEKLY_CAP, Number.NaN).publish.length, 0, "unknown count publishes none");
});

test("SKILL.md names the enforced boundaries and treats findings as untrusted", () => {
  for (const s of ["OS user", "allowRequest", "isolationProblems", "checkScreenshotFiles", "untrusted", "published this week"]) {
    assert.ok(skill.includes(s), s);
  }
});
