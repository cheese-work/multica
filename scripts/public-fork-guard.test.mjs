import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { checkAddedLines, checkPaths, checkText, loadRules } from "./public-fork-guard.mjs";

// The fixtures build every workspace name at run time, so this file carries none of them.
const AGENT = ["x99", "claude", "opus"].join("-");
const PRODUCT_ORG = ["tevo", "studio"].join("-");
const PERSON = ["Che", "ese"].join("");
const rules = loadRules(join(dirname(fileURLToPath(import.meta.url)), "public-fork-guard", "rules.json"));
const SCRIPT = fileURLToPath(new URL("./public-fork-guard.mjs", import.meta.url));
const RULES = fileURLToPath(new URL("./public-fork-guard/rules.json", import.meta.url));

const ids = (hits) => hits.map((h) => h.id).sort();

test("path rule: refuses workspace artifact paths", () => {
  const hits = checkPaths(
    [".plans/CHE-1-plan.md", ".planning/x/y.md", "docs/ux/CHE-2/capture.png", "docs/evidence/CHE-3/a.png", "docs/plans/CHE-4-x.md",
      "evidence/CHE-5-red.txt", "docs/internal/device-broker-policy/a.md", "config/tevo-prod1-guard.json", "docs/engineering/che-6-spike/a.sql", "red2.txt"],
    rules,
  );
  assert.equal(hits.length, 10);
});

test("path rule: accepts ordinary fork paths and admit-sheet files", () => {
  const ok = ["server/main.go", "docs/ux/CHE-7.yml", "docs/fork/PROCESS.md", "packages/views/a.tsx", "scripts/public-fork-guard.mjs", "config.example.json"];
  assert.deepEqual(checkPaths(ok, rules), []);
});

test("text rule: refuses agent, org and person names; accepts the fork's own repository name", () => {
  assert.deepEqual(ids(checkText("t", `fix by ${AGENT}`, rules)), ["agent-name"]);
  assert.deepEqual(ids(checkText("t", `touches ${PRODUCT_ORG}/app`, rules)), ["product-org"]);
  assert.deepEqual(ids(checkText("t", `${PERSON} decided this`, rules)), ["workspace-person"]);
  assert.deepEqual(checkText("t", "see cheese-work/multica#12 and CHE-123", rules), []);
});

test("diff rule: only added lines count, and the report never carries the matched text", () => {
  const diff = [
    "diff --git a/src/a.go b/src/a.go",
    "--- a/src/a.go",
    "+++ b/src/a.go",
    "@@ -1,2 +1,3 @@",
    ` // old line from ${AGENT} stays`,
    `-// removed ${AGENT}`,
    `+// new line from ${AGENT}`,
    "+// clean line",
  ].join("\n");
  const hits = checkAddedLines(diff, rules);
  assert.equal(hits.length, 1);
  assert.deepEqual({ file: hits[0].file, line: hits[0].line, id: hits[0].id }, { file: "src/a.go", line: 2, id: "agent-name" });
  assert.ok(!JSON.stringify(hits).includes(AGENT));
});

test("diff rule: guard files are exempt", () => {
  const diff = ["diff --git a/scripts/public-fork-guard/rules.json b/scripts/public-fork-guard/rules.json", "--- /dev/null", "+++ b/scripts/public-fork-guard/rules.json", "@@ -0,0 +1 @@", `+${AGENT}`].join("\n");
  assert.deepEqual(checkAddedLines(diff, rules), []);
});

test("diff rule: the code-owners file may name its owner handle", () => {
  const owner = ["@cong", "vc-dev"].join("");
  const diff = ["diff --git a/.github/CODEOWNERS b/.github/CODEOWNERS", "--- a/.github/CODEOWNERS", "+++ b/.github/CODEOWNERS", "@@ -1 +1,2 @@", `+/scripts/x.mjs ${owner}`].join("\n");
  assert.deepEqual(checkAddedLines(diff, rules), []);
});

// --- CLI over a real repository ---------------------------------------------

function git(cwd, ...args) {
  const r = spawnSync("git", args, { cwd, encoding: "utf8" });
  assert.equal(r.status, 0, r.stderr);
  return r.stdout.trim();
}
function repo(files) {
  const dir = mkdtempSync(join(tmpdir(), "pfg-"));
  git(dir, "init", "-q", "-b", "main");
  git(dir, "config", "user.email", "t@example.invalid");
  git(dir, "config", "user.name", "t");
  const put = (name, text) => {
    mkdirSync(dirname(join(dir, name)), { recursive: true });
    writeFileSync(join(dir, name), text);
  };
  put("README.md", "base\n");
  git(dir, "add", "-A");
  git(dir, "commit", "-qm", "base");
  const base = git(dir, "rev-parse", "HEAD");
  for (const [name, text] of Object.entries(files)) put(name, text);
  git(dir, "add", "-A");
  git(dir, "commit", "-qm", "head");
  return { dir, base, head: git(dir, "rev-parse", "HEAD") };
}
const cli = ({ dir, base, head }, env = {}) =>
  spawnSync("node", [SCRIPT, "--base", base, "--head", head, "--rules", RULES], { cwd: dir, encoding: "utf8", env: { ...process.env, ...env } });

test("cli: green on a clean change", () => {
  const r = cli(repo({ "src/ok.go": "package ok\n" }));
  assert.equal(r.status, 0, r.stdout + r.stderr);
});

test("cli: red on a workspace artifact path", () => {
  const r = cli(repo({ ".plans/CHE-9-x.md": "plan\n" }));
  assert.equal(r.status, 1);
  assert.match(r.stdout, /path-plans/);
});

test("cli: red on an added line with a workspace name, output holds no matched text", () => {
  const r = cli(repo({ "src/a.go": `// ${AGENT}\n` }));
  assert.equal(r.status, 1);
  assert.match(r.stdout, /src\/a\.go/);
  assert.ok(!r.stdout.includes(AGENT) && !r.stderr.includes(AGENT));
});

test("cli: red on a PR title or body with a name, and on a commit message", () => {
  const clean = repo({ "src/ok.go": "package ok\n" });
  assert.equal(cli(clean, { PR_TITLE: `fix ${AGENT}` }).status, 1);
  assert.equal(cli(clean, { PR_BODY: `${PERSON} asked` }).status, 1);
  const msg = repo({ "src/ok.go": "package ok\n" });
  git(msg.dir, "commit", "-q", "--amend", "-m", `reviewed by ${AGENT}`);
  msg.head = git(msg.dir, "rev-parse", "HEAD");
  assert.equal(cli(msg).status, 1);
});

test("cli: an existing violation outside the change does not fail a clean change", () => {
  // The base already holds a named line; the change adds none. Names are an added-lines rule.
  const dir = mkdtempSync(join(tmpdir(), "pfg-"));
  git(dir, "init", "-q", "-b", "main");
  git(dir, "config", "user.email", "t@example.invalid");
  git(dir, "config", "user.name", "t");
  writeFileSync(join(dir, "old.go"), `// ${AGENT}\n`);
  git(dir, "add", "-A");
  git(dir, "commit", "-qm", "base");
  const base = git(dir, "rev-parse", "HEAD");
  writeFileSync(join(dir, "new.go"), "package n\n");
  git(dir, "add", "-A");
  git(dir, "commit", "-qm", "head");
  assert.equal(cli({ dir, base, head: git(dir, "rev-parse", "HEAD") }).status, 0);
});
