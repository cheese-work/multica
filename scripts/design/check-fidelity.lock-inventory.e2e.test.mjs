// Rules 7, 8, 10, 11 and 12 on real HTML: synthetic lock folders (a LOCK.json and HTML files), one gap each.
// Needs Playwright Chromium (no vite build): the code side is a static page that carries [data-fidelity-root].
//   node --test scripts/design/check-fidelity.lock-inventory.e2e.test.mjs
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

const page = (body) => (body.startsWith("<!doctype") ? body : `<!doctype html><html><head><style>body{margin:0}</style></head><body>${body}</body></html>`);
const root = (state, profile, inner = `<p class="a">Alpha</p>`, extra = "") => `<div data-state="${state}"${profile ? ` data-profile="${profile}"` : ""}${extra}>${inner}</div>`;
const CODE = `<p data-testid="a">Alpha</p>`;

/**
 * files: { name: html body }, all listed in LOCK.json export_files; cases: [{ file, state, profile }].
 * `exportFiles` overrides the list in LOCK.json (default: every file).
 */
function run({ files, cases, exportFiles }) {
  const dir = mkdtempSync(join(tmpdir(), "fidelity-inventory-"));
  const lockDir = join(dir, "approved");
  mkdirSync(lockDir);
  for (const [name, body] of Object.entries(files)) writeFileSync(join(lockDir, name), page(body));
  writeFileSync(join(lockDir, "LOCK.json"), JSON.stringify({ export_files: Object.fromEntries((exportFiles ?? Object.keys(files)).map((n) => [n, "x"])) }));
  const harness = join(dir, "page.html");
  writeFileSync(harness, page(`<div data-fidelity-root>${CODE}</div>`));
  const map = join(dir, "x.blocks.yml");
  writeFileSync(map, [
    "topic: fixture", "cases:",
    ...cases.flatMap((c) => [`  - lock_file: ${join(lockDir, c.file ?? "lock.html")}`, `    state: ${c.state}`, `    profile: ${c.profile}`, "    fixture: none"]),
    "blocks:", "  a:", '    selector: ".a"',
  ].join("\n") + "\n");
  const out = join(dir, "out");
  const r = spawnSync("node", ["scripts/design/check-fidelity.mjs", "--blocks", map, "--out", out, "--harness", harness], { encoding: "utf8" });
  return { r, result: JSON.parse(readFileSync(join(out, "result.json"), "utf8")) };
}
const of = (result, rule) => result.violations.filter((v) => v.rule === rule);
const S1 = { state: "s1", profile: "card-540" };

test("clean lock: every root has a case, every item is claimed: no violation", () => {
  const { r, result } = run({ files: { "lock.html": root("s1", "card-540") + `<p data-annotation>Notes</p>` }, cases: [S1] });
  assert.equal(r.status, 0, r.stderr);
  assert.deepEqual(result.violations, []);
});

test("rule 7 uncovered-state: a marked text-only state with no case", () => {
  const { r, result } = run({ files: { "lock.html": root("s1", "card-540") + root("s2", "card-540", "Only words, no blocks") }, cases: [S1] });
  assert.equal(r.status, 1);
  assert.deepEqual(of(result, "uncovered-state").map((v) => `${v.state}@${v.profile}`), ["s2@card-540"]);
});

test("rule 7 uncovered-state: every phone 358 px case dropped is one instance per phone root", () => {
  const { result } = run({
    files: { "lock.html": root("s1", "card-540") + root("s21", "phone-358") + root("s22", "phone-358") },
    cases: [S1],
  });
  assert.deepEqual(of(result, "uncovered-state").map((v) => v.state), ["s21", "s22"]);
});

test("rule 10 unclaimed-lock-content: an unmarked text-only state is one instance per item", () => {
  const { r, result } = run({ files: { "lock.html": root("s1", "card-540") + `<h3>2 · Text only</h3><div>Just words</div><button>Go</button>` }, cases: [S1] });
  assert.equal(r.status, 1);
  const v = of(result, "unclaimed-lock-content");
  assert.deepEqual(v.map((x) => `${x.item_kind}:${x.item}`), ["text:2 · Text only", "text:Just words", "control:Go"]);
  assert.ok(v.every((x) => x.file.endsWith("lock.html")));
});

test("rule 10: items inside a state root or an annotation are claimed; hidden items are not items", () => {
  const { result } = run({
    files: { "lock.html": root("s1", "card-540") + `<p data-annotation>Note <b>bold</b></p><i style="display:none">Hidden</i>` },
    cases: [S1],
  });
  assert.deepEqual(result.violations, []);
});

test("rule 11 unclaimed-lock-file: a file in export_files with no marks", () => {
  const { r, result } = run({ files: { "lock.html": root("s1", "card-540"), "other.html": `<p>Plain</p>` }, cases: [S1] });
  assert.equal(r.status, 1);
  const v = of(result, "unclaimed-lock-file");
  assert.equal(v.length, 1);
  assert.ok(v[0].file.endsWith("other.html"));
  assert.equal(of(result, "unclaimed-lock-content").length, 1, "its text is also an unclaimed item");
});

test("rule 11: a file whose body is marked data-annotation as a whole is claimed; a file outside export_files is not read", () => {
  const notes = `<!doctype html><html><body data-annotation><p>Notes</p></body></html>`;
  const { result } = run({
    files: { "lock.html": root("s1", "card-540"), "notes.html": notes, "extra.html": `<p>Not exported</p>` },
    cases: [S1],
    exportFiles: ["lock.html", "notes.html"],
  });
  assert.deepEqual(result.violations, []);
});

test("rule 8 profile-mismatch: a case whose profile differs from the root's data-profile", () => {
  const { r, result } = run({ files: { "lock.html": root("s1", "card-540") }, cases: [{ state: "s1", profile: "dm-card-520" }] });
  assert.equal(r.status, 1);
  const v = of(result, "profile-mismatch");
  assert.equal(v.length, 1);
  assert.match(v[0].reason, /data-profile/);
  assert.deepEqual(of(result, "uncovered-state").map((x) => `${x.state}@${x.profile}`), ["s1@card-540"], "a mismatched case covers no root");
});

test("rule 8 profile-mismatch: a profile that is not in devices.yml, and a root whose rendered width is off the profile width", () => {
  const unknown = run({ files: { "lock.html": root("s1", "nope") }, cases: [{ state: "s1", profile: "nope" }] });
  assert.match(of(unknown.result, "profile-mismatch")[0].reason, /not in design\/devices\.yml/);
  const narrow = run({ files: { "lock.html": root("s1", "card-540", `<p class="a">Alpha</p>`, ` style="width:300px"`) }, cases: [S1] });
  const v = of(narrow.result, "profile-mismatch");
  assert.equal(v.length, 1);
  assert.match(v[0].reason, /300/);
});

test("rule 12 bad-marker: an annotation that wraps a state root", () => {
  const { r, result } = run({ files: { "lock.html": `<section data-annotation>Notes ${root("s1", "card-540")}</section>` }, cases: [S1] });
  assert.equal(r.status, 1);
  assert.ok(of(result, "bad-marker").some((v) => /state root/.test(v.reason)), JSON.stringify(of(result, "bad-marker")));
});

test("rule 12 bad-marker: an annotation inside a state root, an annotation with a control, a root with no profile, two roots with one id and profile", () => {
  const { result } = run({
    files: {
      "lock.html": root("s1", "card-540", `<p class="a">Alpha</p><p data-annotation>inside</p>`)
        + `<div data-annotation>Notes <button>Click</button></div>`
        + root("s2", null) + root("s3", "card-540") + root("s3", "card-540"),
    },
    cases: [S1, { state: "s2", profile: "card-540" }, { state: "s3", profile: "card-540" }],
  });
  const reasons = of(result, "bad-marker").map((v) => v.reason).join("\n");
  assert.match(reasons, /inside a state root/);
  assert.match(reasons, /control/);
  assert.match(reasons, /s2.*data-profile|data-profile.*s2/);
  assert.equal(of(result, "bad-marker").filter((v) => /s3/.test(v.reason)).length, 2, "one per duplicate root");
});

test("a lock folder with no LOCK.json is a hard failure, not a silent pass", () => {
  const dir = mkdtempSync(join(tmpdir(), "fidelity-inventory-"));
  mkdirSync(join(dir, "approved"));
  writeFileSync(join(dir, "approved", "lock.html"), page(root("s1", "card-540")));
  const harness = join(dir, "page.html");
  writeFileSync(harness, page(`<div data-fidelity-root>${CODE}</div>`));
  const map = join(dir, "x.blocks.yml");
  writeFileSync(map, `topic: f\ncases:\n  - lock_file: ${join(dir, "approved", "lock.html")}\n    state: s1\n    profile: card-540\n    fixture: none\nblocks:\n  a:\n    selector: ".a"\n`);
  const r = spawnSync("node", ["scripts/design/check-fidelity.mjs", "--blocks", map, "--out", join(dir, "out"), "--harness", harness], { encoding: "utf8" });
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /LOCK\.json/);
});

test("inventory: a second lock folder of the topic whose cases are all removed is still read, in the normal invocation", () => {
  // The map names only design/<topic>/approved; design/<topic>/approved-b has no case at all. No extra argument is passed.
  const topic = `zz-fidelity-fixture-${process.pid}`;
  const topicDir = join(process.cwd(), "design", topic);
  const lock = (name, files) => {
    const d = join(topicDir, name);
    mkdirSync(d, { recursive: true });
    for (const [f, body] of Object.entries(files)) writeFileSync(join(d, f), page(body));
    writeFileSync(join(d, "LOCK.json"), JSON.stringify({ export_files: Object.fromEntries(Object.keys(files).map((n) => [n, "x"])) }));
    return d;
  };
  const tmp = mkdtempSync(join(tmpdir(), "fidelity-inventory-"));
  try {
    const a = lock("approved", { "lock.html": root("s1", "card-540") });
    lock("approved-b", { "other.html": root("s9", "card-540", "Only words") + `<p>Stray</p>` });
    const harness = join(tmp, "page.html");
    writeFileSync(harness, page(`<div data-fidelity-root>${CODE}</div>`));
    const map = join(tmp, "x.blocks.yml");
    writeFileSync(map, `topic: ${topic}\ncases:\n  - lock_file: ${join(a, "lock.html")}\n    state: s1\n    profile: card-540\n    fixture: none\nblocks:\n  a:\n    selector: ".a"\n`);
    const out = join(tmp, "out");
    const r = spawnSync("node", ["scripts/design/check-fidelity.mjs", "--blocks", map, "--out", out, "--harness", harness], { encoding: "utf8" });
    const result = JSON.parse(readFileSync(join(out, "result.json"), "utf8"));
    assert.equal(r.status, 1);
    assert.equal(result.lock_files.length, 2);
    assert.deepEqual(of(result, "uncovered-state").map((v) => `${v.state}@${v.profile}`), ["s9@card-540"]);
    assert.deepEqual(of(result, "unclaimed-lock-content").map((v) => v.item), ["Stray"]);
  } finally {
    rmSync(topicDir, { recursive: true, force: true });
  }
});

test("rule 7 per root: roots s1@card-540 and s1@phone-358 with a case for s1@dm-card-520 leave both roots uncovered", () => {
  const { result } = run({ files: { "lock.html": root("s1", "card-540") + root("s1", "phone-358") }, cases: [{ state: "s1", profile: "dm-card-520" }] });
  assert.deepEqual(of(result, "uncovered-state").map((v) => `${v.state}@${v.profile}`).sort(), ["s1@card-540", "s1@phone-358"]);
  assert.equal(of(result, "profile-mismatch").length, 1);
});
