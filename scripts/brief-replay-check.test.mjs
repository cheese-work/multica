import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, utimesSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { compareBriefs, compareDirs, findRecentBriefs, parseChecklist, checkRealBriefs } from "./brief-replay-check.mjs";

const oldBrief = [
  "## Workflow",
  "3. Set `in_progress` FIRST when the output is the issue's ask.",
  "   Long procedure prose that may move to a reference file.",
  "4. Post the final result with `--content-file`.",
].join("\n");

test("a preserved rule and trigger passes; moved prose is reported, not failed", () => {
  const next = [
    "## Workflow",
    "3. Set `in_progress` FIRST when the output is the issue's ask.",
    "4. Post the final result with `--content-file`.",
  ].join("\n");
  const r = compareBriefs(oldBrief, next, ["in_progress` FIRST", "--content-file"]);
  assert.equal(r.ok, true);
  assert.deepEqual(r.missing, []);
  assert.deepEqual(r.removed, ["Long procedure prose that may move to a reference file."]);
  assert.deepEqual(r.added, []);
});

test("a checklist rule dropped from the new brief fails and is named", () => {
  const next = ["## Workflow", "3. Set status early.", "4. Post the result."].join("\n");
  const r = compareBriefs(oldBrief, next, ["in_progress` FIRST", "--content-file"]);
  assert.equal(r.ok, false);
  assert.deepEqual(r.missing, ["in_progress` FIRST", "--content-file"]);
});

test("a checklist phrase absent from the old brief is flagged as a bad checklist, not a pass", () => {
  const r = compareBriefs(oldBrief, oldBrief, ["never in the brief"]);
  assert.equal(r.ok, false);
  assert.deepEqual(r.notInOld, ["never in the brief"]);
});

test("whitespace and list markers do not count as rule changes", () => {
  const r = compareBriefs("- a   rule\n\n* b rule", "a rule\n  b rule", []);
  assert.deepEqual([r.removed, r.added], [[], []]);
});

test("parseChecklist drops blanks, comments and bullets", () => {
  assert.deepEqual(parseChecklist("# c\n\n- one\n* two\nthree\n"), ["one", "two", "three"]);
});

test("cli exits 1 on a missing rule and 0 on a pass", (t) => {
  const dir = mkdtempSync(join(tmpdir(), "brief-replay-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  writeFileSync(join(dir, "old.md"), "keep this rule\nmove this prose\n");
  writeFileSync(join(dir, "new.md"), "keep this rule\n");
  writeFileSync(join(dir, "ok.txt"), "keep this rule\n");
  writeFileSync(join(dir, "bad.txt"), "keep this rule\nmove this prose\n");
  const cli = fileURLToPath(new URL("./brief-replay-check.mjs", import.meta.url));
  const run = (list) => spawnSync("node", [cli, join(dir, "old.md"), join(dir, "new.md"), join(dir, list)], { encoding: "utf8" });
  assert.equal(run("ok.txt").status, 0);
  const bad = run("bad.txt");
  assert.equal(bad.status, 1);
  assert.match(bad.stdout, /MISSING: move this prose/);
});

test("compareDirs checks a rule only in fixtures whose old brief had it, and fails a vanished fixture", (t) => {
  const dir = mkdtempSync(join(tmpdir(), "brief-replay-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const [a, b] = [join(dir, "old"), join(dir, "new")];
  mkdirSync(a); mkdirSync(b);
  writeFileSync(join(a, "issue.md"), "issue rule\nshared rule\n");
  writeFileSync(join(a, "chat.md"), "shared rule\n");
  writeFileSync(join(b, "issue.md"), "issue rule\nshared rule\n");
  writeFileSync(join(b, "chat.md"), "shared rule\n");
  const pass = compareDirs(a, b, ["issue rule", "shared rule"]);
  assert.equal(pass.ok, true);
  writeFileSync(join(b, "issue.md"), "shared rule\n");
  const fail = compareDirs(a, b, ["issue rule", "shared rule"]);
  assert.equal(fail.ok, false);
  assert.deepEqual(fail.fixtures.map((f) => [f.name, f.missing]), [["chat.md", []], ["issue.md", ["issue rule"]]]);
  rmSync(join(b, "chat.md"));
  assert.equal(compareDirs(a, b, ["shared rule"]).ok, false);
  assert.deepEqual(compareDirs(a, b, ["nowhere"]).notInOld, ["nowhere"]);
});

test("an empty or comment-only checklist fails: it checks no rules", (t) => {
  assert.equal(compareBriefs("critical rule", "", []).ok, false);
  assert.equal(compareBriefs("critical rule", "", []).emptyChecklist, true);
  const dir = mkdtempSync(join(tmpdir(), "brief-replay-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const [a, b] = [join(dir, "old"), join(dir, "new")];
  mkdirSync(a); mkdirSync(b);
  writeFileSync(join(a, "x.md"), "rule\n");
  writeFileSync(join(b, "x.md"), "rule\n");
  assert.equal(compareDirs(a, b, []).ok, false);
  writeFileSync(join(dir, "empty.txt"), "# only a comment\n\n");
  const cli = spawnSync(process.execPath, [fileURLToPath(new URL("./brief-replay-check.mjs", import.meta.url)), a, b, join(dir, "empty.txt")], { encoding: "utf8" });
  assert.equal(cli.status, 1);
  assert.match(cli.stdout, /EMPTY-CHECKLIST/);
});

// Real-brief mode: the platform keeps each run's finished brief (<root>/<ws>/<task>/workdir/CLAUDE.md|AGENTS.md).
const realRoot = (t, briefs) => {
  const root = mkdtempSync(join(tmpdir(), "brief-real-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  briefs.forEach(([task, file, text, age], i) => {
    const d = join(root, "ws", task, "workdir");
    mkdirSync(d, { recursive: true });
    writeFileSync(join(d, file), text);
    utimesSync(join(d, file), new Date(Date.now() - age * 1000), new Date(Date.now() - age * 1000));
  });
  return root;
};

test("findRecentBriefs returns the newest N brief files only, ignoring other files", (t) => {
  const root = realRoot(t, [["a", "CLAUDE.md", "x", 300], ["b", "AGENTS.md", "x", 200], ["c", "CLAUDE.md", "x", 100], ["d", "notes.md", "x", 1]]);
  const got = findRecentBriefs(root, 2).map((f) => f.split("/").at(-3));
  assert.deepEqual(got, ["c", "b"]);
});

test("checkRealBriefs: each checklist phrase must appear in at least one real brief", (t) => {
  const root = realRoot(t, [["a", "CLAUDE.md", "rule one\nchat only rule", 2], ["b", "AGENTS.md", "rule one\nrule two", 1]]);
  const files = findRecentBriefs(root, 10);
  const pass = checkRealBriefs(files, ["rule one", "rule two", "chat only rule"]);
  assert.equal(pass.ok, true);
  assert.deepEqual(pass.counts, [["rule one", 2], ["rule two", 1], ["chat only rule", 1]]);
  const fail = checkRealBriefs(files, ["rule one", "invented rule"]);
  assert.equal(fail.ok, false);
  assert.deepEqual(fail.neverSeen, ["invented rule"]);
});

test("checkRealBriefs fails with no briefs or an empty checklist", () => {
  assert.equal(checkRealBriefs([], ["rule"]).ok, false);
  assert.equal(checkRealBriefs([], ["rule"]).noBriefs, true);
  assert.equal(checkRealBriefs([], []).ok, false);
  assert.equal(checkRealBriefs([], []).emptyChecklist, true);
});

test("cli --real exits 0 when every phrase is in a real brief and 1 when one never is; prints no brief text", (t) => {
  const root = realRoot(t, [["a", "CLAUDE.md", "rule one SECRET-ISSUE-TEXT", 1]]);
  writeFileSync(join(root, "ok.txt"), "rule one\n");
  writeFileSync(join(root, "bad.txt"), "rule one\ninvented rule\n");
  const cli = fileURLToPath(new URL("./brief-replay-check.mjs", import.meta.url));
  const run = (l) => spawnSync(process.execPath, [cli, "--real", join(root, l), root, "5"], { encoding: "utf8" });
  const ok = run("ok.txt");
  assert.equal(ok.status, 0);
  assert.doesNotMatch(ok.stdout, /SECRET-ISSUE-TEXT/);
  const bad = run("bad.txt");
  assert.equal(bad.status, 1);
  assert.match(bad.stdout, /NEVER-SEEN: invented rule/);
});
