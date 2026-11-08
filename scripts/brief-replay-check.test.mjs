import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { compareBriefs, compareDirs, parseChecklist } from "./brief-replay-check.mjs";

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
