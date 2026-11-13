import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import test from "node:test";
import { resolve } from "node:path";

import { parsePatterns, validatePatterns, validateRepo } from "./validate-patterns.mjs";

const URL_OK = "https://doop.example/c/mRAnnjvGod?frame=Endq7SreMD";
const DESIGN_MD = `
## Approved revisions

| Topic | Revision | Direction | Design PR | Revision commit SHA |
| --- | --- | --- | --- | --- |
| decision-card | doop \`Z3bizs5RyO\` @ 2026-11-12T04:18:27.430Z (B v9) | B | 1379 | n/a |
| wakeups-pause | 3 | A | 255 | \`de646d2bb8c5\` |
`;
const exists = new Set(["packages/ui/components/ui/button.tsx", "packages/ui/docs/button.md"]);
const keys = new Set(["issues:reply.annotations.add"]);
const ctx = {
  designMd: DESIGN_MD,
  pathExists: (p) => exists.has(p),
  i18nHas: (ns, key) => keys.has(`${ns}:${key}`),
};

const entry = (over = {}) => ({
  id: "button",
  intent: "One dominant action per dialog or form.",
  origin: "upstream",
  status: "active",
  components: ["packages/ui/components/ui/button.tsx"],
  frame: { state: "patterns", url: URL_OK },
  ...over,
});
const run = (entries, base = null, extra = {}) =>
  validatePatterns({ entries, base, ...ctx, ...extra });
const errorsOf = (entries, base) => run(entries, base).errors.join("\n");

test("accepts a complete entry with docs and copy", () => {
  const r = run([
    entry({
      docs: ["packages/ui/docs/button.md"],
      copy: { trigger: "issues:reply.annotations.add" },
    }),
  ]);
  assert.deepEqual(r.errors, []);
  assert.deepEqual(r.warnings, []);
});

test("rejects a missing component path and names the entry and path", () => {
  const e = errorsOf([entry({ components: ["packages/ui/components/ui/nope.tsx"] })]);
  assert.match(e, /button/);
  assert.match(e, /packages\/ui\/components\/ui\/nope\.tsx/);
});

test("rejects a missing docs path", () => {
  assert.match(errorsOf([entry({ docs: ["packages/ui/docs/nope.md"] })]), /docs.*nope\.md/);
});

test("rejects an unknown i18n key and a key without a namespace", () => {
  assert.match(errorsOf([entry({ copy: { trigger: "issues:reply.annotations.nope" } })]), /issues:reply\.annotations\.nope/);
  assert.match(errorsOf([entry({ copy: { trigger: "reply.annotations.add" } })]), /namespace/);
});

test("rejects a malformed frame link on state patterns", () => {
  for (const url of [undefined, "", "not a url", "http://doop.example/c/abc?frame=x", "https://doop.example/c/abc", "https://doop.example/c/abc?frame="]) {
    assert.match(errorsOf([entry({ frame: { state: "patterns", url } })]), /frame/, String(url));
  }
});

test("pending warns and does not fail; other frame fields are rejected", () => {
  const r = run([entry({ frame: { state: "pending" } })]);
  assert.deepEqual(r.errors, []);
  assert.equal(r.warnings.length, 1);
  assert.match(r.warnings[0], /button.*pending/);
  assert.match(errorsOf([entry({ frame: { state: "pending", url: URL_OK } })]), /pending.*url/);
});

test("locked passes with a matching approved-revisions row", () => {
  const e = entry({ frame: { state: "locked", topic: "decision-card", doop_version: "Z3bizs5RyO", frames: ["Abc123"] } });
  assert.deepEqual(run([e]).errors, []);
});

test("locked fails for an unknown topic, an unknown version or missing frames", () => {
  const lock = (over) => entry({ frame: { state: "locked", topic: "decision-card", doop_version: "Z3bizs5RyO", frames: ["Abc123"], ...over } });
  assert.match(errorsOf([lock({ topic: "no-such-topic" })]), /no-such-topic/);
  assert.match(errorsOf([lock({ doop_version: "WRONGVERSION" })]), /WRONGVERSION/);
  assert.match(errorsOf([lock({ topic: "wakeups-pause" })]), /Z3bizs5RyO|wakeups-pause/);
  assert.match(errorsOf([lock({ frames: [] })]), /frames/);
  assert.match(errorsOf([lock({ topic: undefined })]), /topic/);
});

test("allowed moves: pending→patterns, locked→patterns, no change", () => {
  const locked = { state: "locked", topic: "decision-card", doop_version: "Z3bizs5RyO", frames: ["F1"] };
  const base = [entry({ id: "a", frame: { state: "pending" } }), entry({ id: "b", frame: locked }), entry({ id: "c", frame: { state: "patterns", url: URL_OK } })];
  const next = [entry({ id: "a" }), entry({ id: "b" }), entry({ id: "c" })];
  assert.deepEqual(run(next, base).errors, []);
  assert.deepEqual(run(base, base).errors, []);
});

test("forbidden moves: patterns→locked, locked→pending, pending→locked", () => {
  const locked = { state: "locked", topic: "decision-card", doop_version: "Z3bizs5RyO", frames: ["F1"] };
  const states = { pending: { state: "pending" }, locked, patterns: { state: "patterns", url: URL_OK } };
  for (const [from, to] of [["patterns", "locked"], ["locked", "pending"], ["pending", "locked"], ["patterns", "pending"]]) {
    const e = errorsOf([entry({ frame: states[to] })], [entry({ frame: states[from] })]);
    assert.match(e, new RegExp(`${from}.*${to}`), `${from}→${to}`);
  }
});

test("a new entry may not start as pending once a base ledger exists, but may seed with no base", () => {
  assert.match(errorsOf([entry({ id: "new" })].map((e) => ({ ...e, frame: { state: "pending" } })), [entry({ id: "other" })]), /new.*pending/);
  assert.deepEqual(run([entry({ frame: { state: "pending" } })], null).errors, []);
});

test("rejects duplicate ids, bad enums, missing fields and unknown fields", () => {
  assert.match(errorsOf([entry(), entry()]), /duplicate.*button/i);
  assert.match(errorsOf([entry({ origin: "other" })]), /origin/);
  assert.match(errorsOf([entry({ status: "gone" })]), /status/);
  assert.match(errorsOf([entry({ intent: undefined })]), /intent/);
  assert.match(errorsOf([entry({ id: "Bad Id" })]), /id/);
  assert.match(errorsOf([entry({ colour: "red" })]), /colour/);
});

test("parsePatterns reads a YAML list and rejects non-lists", () => {
  assert.equal(parsePatterns("- id: a\n- id: b\n").length, 2);
  assert.throws(() => parsePatterns("id: a\n"), /list/);
});

test("the real ledger passes against the real repo", () => {
  const r = validateRepo(resolve(import.meta.dirname, "../.."));
  assert.deepEqual(r.errors, []);
  assert.ok(r.count >= 40, "S1 seeded 40 entries; the ledger only grows");
});

test("DESIGN.md links to the ledger and no longer carries the pattern table", () => {
  const md = readFileSync(resolve(import.meta.dirname, "../../DESIGN.md"), "utf8");
  assert.match(md, /design\/patterns\.yml/);
  assert.doesNotMatch(md, /\| Pattern \| Source \| Rule \|/);
});

test("CLI output adds no line the CI warning budget counts as a warning", () => {
  const cli = resolve(import.meta.dirname, "validate-patterns.mjs");
  const run = spawnSync(process.execPath, [cli], { encoding: "utf8" });
  assert.equal(run.status, 0, run.stderr);
  // Same pattern as scripts/warning-budget.mjs: pending frames must not add a new CI warning signature.
  const counted = /##\[(warning|notice)\]|::(?:warning|notice)::|\b(?:DeprecationWarning|ExperimentalWarning|[Ww]arning[:!]|WARN(?:ING)?\b)|\bnpm warn\b|⚠|╭ Warning |Found \d+ warnings|\d+:\d+\s+warning\b/;
  const lines = `${run.stdout}\n${run.stderr}`.split("\n").filter((l) => counted.test(l));
  assert.deepEqual(lines, []);
  assert.match(run.stdout, /pending/);
});

test("the three pilot patterns link a frame on the Patterns page (CHE-5428)", () => {
  const entries = parsePatterns(readFileSync(resolve(import.meta.dirname, "../../design/patterns.yml"), "utf8"));
  const pilots = ["icon-button-action", "property-picker", "issue-mention-chip"];
  for (const id of pilots) {
    const frame = entries.find((e) => e.id === id)?.frame;
    assert.equal(frame?.state, "patterns", `${id} frame.state`);
    assert.match(frame?.url ?? "", /^https:\/\/[^/]+\/c\/mRAnnjvGod\?frame=[\w-]+$/, `${id} frame.url`);
  }
});

test("batch F3 patterns link a frame on the Patterns page (CHE-5528)", () => {
  const entries = parsePatterns(readFileSync(resolve(import.meta.dirname, "../../design/patterns.yml"), "utf8"));
  const batch = ["decision-action-approval", "description-disclosure", "status-pill", "header-chip-popover", "form-dialog"];
  for (const id of batch) {
    const frame = entries.find((e) => e.id === id)?.frame;
    assert.equal(frame?.state, "patterns", `${id} frame.state`);
    assert.match(frame?.url ?? "", /^https:\/\/[^/]+\/c\/mRAnnjvGod\?frame=[\w-]+$/, `${id} frame.url`);
  }
});

test("batch F2 patterns link a frame on the Patterns page (CHE-5527)", () => {
  const entries = parsePatterns(readFileSync(resolve(import.meta.dirname, "../../design/patterns.yml"), "utf8"));
  const batch = ["lazy-detail", "decision-receipt-card", "collapsible-section", "settings-select-row", "pill-menu"];
  for (const id of batch) {
    const frame = entries.find((e) => e.id === id)?.frame;
    assert.equal(frame?.state, "patterns", `${id} frame.state`);
    assert.match(frame?.url ?? "", /^https:\/\/[^/]+\/c\/mRAnnjvGod\?frame=[\w-]+$/, `${id} frame.url`);
  }
});

test("the four batch F6 patterns link a frame on the Patterns page (CHE-5531)", () => {
  const entries = parsePatterns(readFileSync(resolve(import.meta.dirname, "../../design/patterns.yml"), "utf8"));
  for (const id of ["section-eyebrow", "type-role-tokens", "inline-link-action", "choice-card"]) {
    const frame = entries.find((e) => e.id === id)?.frame;
    assert.equal(frame?.state, "patterns", `${id} frame.state`);
    assert.match(frame?.url ?? "", /^https:\/\/[^/]+\/c\/mRAnnjvGod\?frame=[\w-]+$/, `${id} frame.url`);
  }
});

test("batch F5 patterns link their captured frames on the Patterns page (CHE-5530)", () => {
  const entries = parsePatterns(readFileSync(resolve(import.meta.dirname, "../../design/patterns.yml"), "utf8"));
  const frames = {
    "integration-card-grid": "DuqxVys-Eo",
    "settings-table-section": "zwtKWV4hl7",
    "settings-action-row": "sG7c6FoEh5",
    "create-shell": "LqGJiAq4L4",
    "confirm-dialog": "Ii5sqw8rC5",
  };
  for (const [id, frameId] of Object.entries(frames)) {
    const frame = entries.find((e) => e.id === id)?.frame;
    assert.equal(frame?.state, "patterns", `${id} frame.state`);
    assert.equal(frame?.url, `https://congvc-c00.taila6fa8a.ts.net:34443/c/mRAnnjvGod?frame=${frameId}`, `${id} frame.url`);
  }
});
