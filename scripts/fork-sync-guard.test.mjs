import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { checkForkSync, collectForkSync, isUpstreamSync } from "./fork-sync-guard.mjs";

const fixture = JSON.parse(readFileSync(new URL("./fixtures/fork-sync-che867.json", import.meta.url), "utf8"));
const oldUI = "packages/views/settings/components/github-tab.tsx";
const oldTest = "packages/views/settings/components/github-tab.test.tsx";
const newUI = "packages/views/settings/components/code-tab.tsx";
const newTest = "packages/views/settings/components/code-tab.test.tsx";
const ported = `Ported: ${oldUI} -> ${newUI}\nPorted: ${oldTest} -> ${newTest}`;

test("CHE-867 deletion fixture fails with H01 and both deleted paths", () => {
  const result = checkForkSync(fixture);
  assert.equal(result.sync, true);
  assert.equal(result.violations.length, 2, "CHE-867 deleted the H01 UI and its fork-authored guarding test");
  assert.match(result.violations.join("\n"), new RegExp(`H01.*${oldUI}`));
  assert.match(result.violations.join("\n"), new RegExp(`fork-authored test.*${oldTest}`));
});

test("CHE-867 Ported: lines pass when both replacement files exist", () => {
  assert.deepEqual(checkForkSync({ ...fixture, body: `${fixture.body}\n${ported}` }).violations, []);
});

test("normal non-sync PRs are not affected by identical deletions", () => {
  assert.deepEqual(checkForkSync({ ...fixture, title: "refactor(settings): remove legacy UI" }), { sync: false, violations: [] });
});

for (const input of [
  { headRef: "upstream-sync/v0.8.0" },
  { title: "CHE-867: sync upstream multica v0.6.0" },
  { title: "chore(fork): sync upstream multica v0.7.0 (CHE-2005) [merge]" },
  { title: "chore(multica): merge upstream v0.7.1 (CHE-2241) [merge]" },
  { changes: [{ status: "M", path: "UPSTREAM_VERSION" }] },
]) {
  test(`sync detection: ${JSON.stringify(input)}`, () => assert.equal(isUpstreamSync(input), true));
}

test("a title about the sync guard itself is not a sync", () => {
  assert.equal(isUpstreamSync({ title: "ci(fork): guard upstream sync deletions (CHE-2412) [squash]" }), false);
});

test("renames protect the previous Source and test paths", () => {
  const changes = [
    { status: "R100", previousPath: oldUI, path: newUI },
    { status: "R075", previousPath: oldTest, path: newTest },
  ];
  assert.equal(checkForkSync({ ...fixture, changes }).violations.length, 2);
  assert.deepEqual(checkForkSync({ ...fixture, changes, body: ported }).violations, []);
});

test("each lost file needs its own Ported: line with an existing destination", () => {
  assert.equal(checkForkSync({ ...fixture, body: `Ported: ${oldUI} -> ${newUI}` }).violations.length, 1);
  assert.equal(checkForkSync({ ...fixture, body: ported, headFiles: [] }).violations.length, 2);
  assert.equal(checkForkSync({ ...fixture, body: `Ported: ${oldUI} -> ${oldUI}` }).violations.length, 2);
});

test("backtick paths and Markdown list acknowledgements are accepted", () => {
  const body = `- Ported: \`${oldUI}\` -> \`${newUI}\`\n* Ported: \`${oldTest}\` -> \`${newTest}\``;
  assert.deepEqual(checkForkSync({ ...fixture, body }).violations, []);
});

test("quoted, fenced and commented examples do not acknowledge ports", () => {
  for (const body of [`> ${ported.replaceAll("\n", "\n> ")}`, `\`\`\`text\n${ported}\n\`\`\``, `~~~\n${ported}\n~~~`, `<!--\n${ported}\n-->`]) {
    assert.equal(checkForkSync({ ...fixture, body }).violations.length, 2);
  }
});

test("Source directory, wildcard, shorthand and shared-feature paths are protected", () => {
  const ledger = [
    "| C01 Disclosure | Source: `packages/views/issues/components/thread.ts`, `description.tsx`. |",
    "| G01 Budgets | Source: `server/internal/governance/budget*.go`. |",
    "| O01 CI | Source: `.github/workflows/`. |",
    "| O02 Shared CI | Source: `.github/workflows/check.yml`. |",
  ].join("\n");
  const changes = ["packages/views/issues/components/description.tsx", "server/internal/governance/budget_check.go", ".github/workflows/check.yml"]
    .map((path) => ({ status: "D", path }));
  const result = checkForkSync({ ...fixture, ledger, changes });
  assert.equal(result.violations.length, 3);
  assert.match(result.violations[0], /C01/);
  assert.match(result.violations[1], /G01/);
  assert.match(result.violations[2], /O01, O02/);
});

test("modifications and unrelated upstream test deletions pass", () => {
  const changes = [{ status: "M", path: oldUI }, { status: "D", path: "upstream/unchanged.test.ts" }];
  assert.deepEqual(checkForkSync({ ...fixture, changes }).violations, []);
});

test("a shorthand Source filename after a directory stays in that directory", () => {
  const ledger = "| H01 GitHub | Source: `server/internal/handler/`, `github.go`. |";
  const result = checkForkSync({ ...fixture, ledger, changes: [{ status: "D", path: "server/internal/github.go" }] });
  assert.deepEqual(result.violations, []);
});

test("the live H01 ledger protects its backend and repositories UI sources", () => {
  const ledger = readFileSync(new URL("../docs/fork/FORK-FEATURES.md", import.meta.url), "utf8");
  const changes = ["server/internal/handler/github.go", "packages/views/settings/components/repositories-section.tsx"]
    .map((path) => ({ status: "D", path }));
  const result = checkForkSync({ ...fixture, ledger, changes });
  assert.equal(result.violations.length, 2);
  assert.ok(result.violations.every((violation) => violation.includes("H01")));
});

function repositoryFixture(context) {
  const cwd = mkdtempSync(join(tmpdir(), "fork-sync-guard-"));
  context.after(() => rmSync(cwd, { recursive: true, force: true }));
  const git = (...args) => execFileSync("git", ["-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=/dev/null", ...args], { cwd, encoding: "utf8" });
  const write = (path, text) => {
    mkdirSync(dirname(join(cwd, path)), { recursive: true });
    writeFileSync(join(cwd, path), text);
  };
  const commit = () => {
    git("add", ".");
    git("commit", "--no-gpg-sign", "-qm", "fixture");
    return git("rev-parse", "HEAD").trim();
  };
  git("init", "--quiet", "--template=", "--initial-branch=main");
  write("UPSTREAM_VERSION", "1.0.0\n");
  write(newUI, "upstream replacement UI\n");
  write(newTest, "upstream replacement test\n");
  write("server/upstream_test.go", "upstream assertion\n");
  write("server/modified_test.go", "upstream assertion\n");
  commit();
  git("tag", "v1.0.0");
  write("docs/fork/FORK-FEATURES.md", fixture.ledger);
  write(oldUI, "fork multi-org UI\n");
  write(oldTest, "fork multi-org test\n");
  write("server/modified_test.go", "fork assertion\n");
  write("tests/test_policy.py", "fork policy assertion\n");
  write("e2e/fork.spec.ts", "fork browser assertion\n");
  const base = commit();
  git("rm", oldUI, oldTest, "server/upstream_test.go", "server/modified_test.go", "tests/test_policy.py");
  git("mv", "e2e/fork.spec.ts", "e2e/ported.spec.ts");
  write("docs/fork/FORK-FEATURES.md", "PR head erased the ledger\n");
  write("scripts/fork-sync-guard.mjs", 'throw new Error("untrusted PR code executed");\n');
  const head = commit();
  const pullRequest = { title: fixture.title, body: null, base: { sha: base }, head: { sha: head, ref: fixture.headRef } };
  return { cwd, git, write, pullRequest };
}

test("Git collection uses the base ledger and distinguishes fork tests from untouched upstream tests", (context) => {
  const { cwd, pullRequest } = repositoryFixture(context);
  const input = collectForkSync(pullRequest, cwd);
  assert.equal(input.ledger, fixture.ledger);
  assert.ok(input.forkTests.includes(oldTest));
  assert.ok(input.forkTests.includes("server/modified_test.go"));
  assert.ok(input.forkTests.includes("tests/test_policy.py"));
  assert.ok(input.forkTests.includes("e2e/fork.spec.ts"));
  assert.ok(!input.forkTests.includes("server/upstream_test.go"));
  const result = checkForkSync(input);
  assert.equal(result.violations.length, 5);
  assert.match(result.violations.join("\n"), /e2e\/fork.spec.ts was renamed to e2e\/ported.spec.ts/);
  assert.ok(!result.violations.some((violation) => violation.includes("server/upstream_test.go")));
});

test("CLI fails on losses, passes Ported: acknowledgements, and never executes PR-head code", (context) => {
  const { cwd, git, write, pullRequest } = repositoryFixture(context);
  const eventPath = join(cwd, "event.json");
  const script = fileURLToPath(new URL("./fork-sync-guard.mjs", import.meta.url));
  const run = () => {
    write("event.json", JSON.stringify({ pull_request: pullRequest }));
    return spawnSync(process.execPath, [script], { cwd, env: { ...process.env, GITHUB_EVENT_PATH: eventPath }, encoding: "utf8" });
  };
  const red = run();
  assert.equal(red.status, 1);
  assert.match(red.stderr, /Fork sync guard FAILED/);
  assert.ok(red.stderr.includes(`H01: ${oldUI} was deleted`));
  assert.ok(red.stderr.includes(`fork-authored test: ${oldTest} was deleted`));
  pullRequest.body = `${ported}\nPorted: server/modified_test.go -> ${newTest}\nPorted: tests/test_policy.py -> ${newTest}\nPorted: e2e/fork.spec.ts -> e2e/ported.spec.ts`;
  const green = run();
  assert.equal(green.status, 0, green.stderr);
  assert.match(green.stdout, /Fork sync guard PASSED/);
  assert.equal(git("rev-parse", "HEAD").trim(), pullRequest.head.sha);
});

test("missing upstream tag fails closed for syncs but does not affect ordinary PRs", (context) => {
  const { cwd, git, pullRequest } = repositoryFixture(context);
  git("tag", "-d", "v1.0.0");
  assert.throws(() => collectForkSync(pullRequest, cwd), /refs\/tags\/v1.0.0/);
  assert.deepEqual(checkForkSync(collectForkSync({ ...pullRequest, title: "refactor: cleanup" }, cwd)), { sync: false, violations: [] });
});

test("a sync with no parseable Source paths fails closed", () => {
  assert.throws(() => checkForkSync({ ...fixture, ledger: "" }), /No Source paths/);
});

test("the workflow uses trusted code on a hosted runner and reruns after body edits", () => {
  const workflow = readFileSync(new URL("../.github/workflows/fork-sync-guard.yml", import.meta.url), "utf8");
  assert.match(workflow, /pull_request_target:/);
  assert.match(workflow, /types: \[opened, edited, synchronize, reopened\]/);
  assert.match(workflow, /ref: \$\{\{ github.workflow_sha \}\}/);
  assert.match(workflow, /runs-on: ubuntu-24.04/);
  assert.match(workflow, /persist-credentials: false/);
  assert.match(workflow, /fetch-depth: 0/);
  assert.match(workflow, /test "\$\(git rev-parse FETCH_HEAD\)" = "\$EXPECTED_HEAD"/);
  assert.doesNotMatch(workflow, /self-hosted|secrets\.|pull_request\.head\.repo/);
  assert.match(workflow, /node scripts\/fork-sync-guard.mjs/);
});
