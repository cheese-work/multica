// CHE-1676: X99 runner workspaces are reused. A job that sparse-checks one file leaves
// core.sparseCheckout=true in .git/config; actions/checkout then disables sparse mode in
// config.worktree and unsets extensions.worktreeConfig, which hides that setting and leaves
// the next full checkout without its files. Every checkout runs a heal step first.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

const WORKFLOWS = ["ci.yml", "pr-changelog-check.yml", "pr-close-target-check.yml"];
const STEP = "- name: Clear stale sparse-checkout state";
const read = (name) => readFileSync(new URL(`../.github/workflows/${name}`, import.meta.url), "utf8");

// The first heal step's `run: |` body, dedented. Empty when the workflow has none.
function healScript() {
  const lines = read("ci.yml").split("\n");
  const start = lines.findIndex((l) => l.trim() === STEP);
  if (start < 0) return "";
  const run = lines.findIndex((l, i) => i > start && l.trim() === "run: |");
  const indent = lines[run].search(/\S/) + 2;
  const body = [];
  for (const l of lines.slice(run + 1)) {
    if (l.trim() !== "" && l.search(/\S/) < indent) break;
    body.push(l.slice(indent));
  }
  return body.join("\n");
}

const git = (cwd, ...args) =>
  execFileSync("git", ["-c", "user.name=t", "-c", "user.email=t@t", ...args], { cwd, encoding: "utf8" });
const FILES = ["scripts/x.mjs", "scripts/y.mjs", ".github/z.json", "top.txt"];

function fixture() {
  const dir = mkdtempSync(join(tmpdir(), "sparse-heal-"));
  git(dir, "init", "-q");
  for (const f of FILES) {
    mkdirSync(join(dir, f, ".."), { recursive: true });
    writeFileSync(join(dir, f), f);
  }
  git(dir, "add", ".");
  git(dir, "commit", "-qm", "init");
  return dir;
}

// Leaves the runner workspace as the close-target / changelog jobs did.
function poison(dir) {
  git(dir, "config", "extensions.worktreeConfig", "true");
  git(dir, "config", "core.sparseCheckout", "true");
  mkdirSync(join(dir, ".git/info"), { recursive: true });
  writeFileSync(join(dir, ".git/info/sparse-checkout"), "\n/scripts/x.mjs\n");
  git(dir, "read-tree", "-mu", "HEAD");
  assert.deepEqual(FILES.filter((f) => existsSync(join(dir, f))), ["scripts/x.mjs"]);
}

// What actions/checkout does for a job without a `sparse-checkout` input.
function checkoutWithoutSparse(dir) {
  git(dir, "sparse-checkout", "disable");
  git(dir, "config", "--local", "--unset-all", "extensions.worktreeConfig");
  git(dir, "checkout", "--force", "HEAD");
}

const heal = (dir) => {
  const script = healScript();
  if (script) execFileSync("bash", ["-e", "-c", script], { env: { ...process.env, GITHUB_WORKSPACE: dir } });
};
const present = (dir) => FILES.filter((f) => existsSync(join(dir, f)));

test("a clean workspace keeps every file through heal and checkout", () => {
  const dir = fixture();
  heal(dir);
  checkoutWithoutSparse(dir);
  assert.deepEqual(present(dir), FILES);
  rmSync(dir, { recursive: true });
});

test("a workspace with no repository yet is left alone", () => {
  const dir = mkdtempSync(join(tmpdir(), "sparse-heal-empty-"));
  assert.doesNotThrow(() => heal(dir));
  rmSync(dir, { recursive: true });
});

test("a poisoned workspace gets its files back after heal and checkout", () => {
  const dir = fixture();
  poison(dir);
  heal(dir);
  checkoutWithoutSparse(dir);
  assert.deepEqual(present(dir), FILES);
  rmSync(dir, { recursive: true });
});

test("heal leaves no sparse setting behind to poison the next checkout", () => {
  const dir = fixture();
  poison(dir);
  heal(dir);
  checkoutWithoutSparse(dir);
  checkoutWithoutSparse(dir);
  assert.deepEqual(present(dir), FILES);
  const setting = git(dir, "config", "--local", "--default", "unset", "--get", "core.sparseCheckout").trim();
  assert.notEqual(setting, "true");
  rmSync(dir, { recursive: true });
});

for (const name of WORKFLOWS) {
  test(`${name} runs the heal step before every checkout and sparse-checks nothing`, () => {
    const lines = read(name).split("\n");
    const checkouts = lines.flatMap((l, i) => (l.includes("uses: actions/checkout@") ? [i] : []));
    assert.ok(checkouts.length > 0);
    for (const i of checkouts) {
      const stepStart = lines.slice(0, i).findLastIndex((l) => /^\s+- /.test(l));
      const before = lines.slice(0, stepStart).findLastIndex((l) => /^\s+- /.test(l));
      assert.equal(lines[before]?.trim(), STEP, `checkout at line ${i + 1}`);
    }
    assert.doesNotMatch(read(name), /sparse-checkout/);
  });
}
