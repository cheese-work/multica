// Pins what an agent reads cold in the verify-multica skill (CHE-6602): the
// frontmatter, the five sections, the helpers, the feature index, and that the
// cleanup helper refuses a pid it did not start.
// Run: node --test .agents/skills/verify-multica/scripts/skill.test.mjs
import { strict as assert } from "node:assert";
import { spawn, spawnSync } from "node:child_process";
import { accessSync, constants, existsSync, readFileSync, readdirSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const scripts = dirname(fileURLToPath(import.meta.url));
const skill = resolve(scripts, "..");
const root = resolve(skill, "../../..");
const text = (p) => readFileSync(join(skill, p), "utf8");

test("frontmatter names the skill and says when to use it", () => {
  const m = /^---\nname: verify-multica\ndescription: (.+)\n---\n/.exec(text("SKILL.md"));
  assert.ok(m, "SKILL.md must open with name: verify-multica and a one-line description");
  assert.match(m[1], /^Use when /);
});

test("SKILL.md has Launch, Doctor, Drive, Evidence and Cleanup, in that order", () => {
  const headings = [...text("SKILL.md").matchAll(/^## (.+)$/gm)].map((m) => m[1]);
  const wanted = ["Launch", "Doctor", "Drive", "Evidence", "Cleanup"];
  assert.deepEqual(headings.filter((h) => wanted.includes(h)), wanted);
});

test("every helper SKILL.md names exists and is executable", () => {
  const named = new Set([...text("SKILL.md").matchAll(/scripts\/([\w.-]+\.sh)/g)].map((m) => m[1]));
  assert.ok(named.size >= 4, "launch, doctor, drive and cleanup helpers are documented");
  for (const name of named) {
    const path = join(skill, "scripts", name);
    assert.ok(existsSync(path), `${name} is documented but missing`);
    accessSync(path, constants.X_OK);
  }
});

test("the feature index links every feature file and every link resolves", () => {
  const index = text("features/README.md");
  const links = [...index.matchAll(/\]\(([\w-]+\.md)\)/g)].map((m) => m[1]);
  const files = readdirSync(join(skill, "features")).filter((f) => f !== "README.md");
  assert.deepEqual([...links].sort(), files.sort());
  for (const want of ["decision-cards.md", "notification-settings.md", "workflow-status.md"]) {
    assert.ok(links.includes(want), `${want} is a seed feature`);
  }
});

test("each feature file has its sections and links its FORK-FEATURES.md row", () => {
  for (const f of ["decision-cards.md", "notification-settings.md", "workflow-status.md"]) {
    const body = text(`features/${f}`);
    for (const h of ["What it does", "How to reach it", "Settings", "States", "Edge cases", "Ledger row"]) {
      assert.match(body, new RegExp(`^## ${h}`, "m"), `${f} lacks ## ${h}`);
    }
    for (const surface of ["Web", "Desktop", "CLI", "Mobile"]) {
      assert.match(body, new RegExp(`\\*\\*${surface}\\*\\*`), `${f} lacks the ${surface} surface`);
    }
    assert.match(body, /docs\/fork\/FORK-FEATURES\.md/, `${f} lacks the ledger link`);
  }
});

test("the ledger and the sync guard are not edited by this skill", () => {
  assert.ok(existsSync(join(root, "docs/fork/FORK-FEATURES.md")));
  assert.ok(existsSync(join(root, "scripts/fork-sync-guard.mjs")));
});

test("stop-pid.sh refuses a pid whose cwd is outside the expected directory", async () => {
  // A process this test starts in an unrelated directory: cleanup must not touch it.
  const child = spawn(process.execPath, ["-e", "setTimeout(() => {}, 30000)"], { cwd: "/", stdio: "ignore" });
  try {
    const r = spawnSync(join(skill, "scripts/stop-pid.sh"), [String(child.pid), root], { encoding: "utf8" });
    assert.notEqual(r.status, 0, "stop-pid.sh must exit non-zero");
    assert.match(r.stderr, /refus/i);
    assert.equal(spawnSync("kill", ["-0", String(child.pid)]).status, 0, "the process is still alive");
  } finally {
    child.kill();
  }
});

test("stop-pid.sh stops a pid whose cwd is inside the expected directory", async () => {
  const child = spawn(process.execPath, ["-e", "setTimeout(() => {}, 30000)"], { cwd: skill, stdio: "ignore" });
  const exited = new Promise((r) => child.once("exit", r));
  const r = spawnSync(join(skill, "scripts/stop-pid.sh"), [String(child.pid), skill], { encoding: "utf8" });
  assert.equal(r.status, 0, r.stderr);
  await exited;
});

test("stop-pid.sh refuses a non-numeric pid and the multica daemon's own name", () => {
  const r = spawnSync(join(skill, "scripts/stop-pid.sh"), ["multica", root], { encoding: "utf8" });
  assert.notEqual(r.status, 0);
});
