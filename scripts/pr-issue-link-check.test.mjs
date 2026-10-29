import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

const script = new URL("./pr-close-target-check.mjs", import.meta.url).pathname;

function run(overrides = {}) {
  const directory = mkdtempSync(join(tmpdir(), "pr-issue-link-"));
  try {
    const event = join(directory, "event.json");
    writeFileSync(event, JSON.stringify({
      pull_request: {
        number: 307,
        title: "docs: lock design [squash]",
        head: { ref: "design/example" },
        body: "",
        user: { login: "contributor" },
        ...overrides,
      },
    }));
    return spawnSync(process.execPath, [script, "--event", event], { encoding: "utf8" });
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
}

test("a zero-key PR fails with an actionable issue-link fix", () => {
  const result = run();
  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stdout, /::error::/);
  assert.match(result.stdout, /Part of CHE-N/);
  assert.match(result.stdout, /title.*whole issue/i);
});

for (const { name, overrides } of [
  { name: "utf-8 title", overrides: { title: "fix: utf-8 decoding [squash]" } },
  { name: "sha-256 title", overrides: { title: "chore: pin sha-256 digests [squash]" } },
  { name: "node-22 title", overrides: { title: "build: move to node-22 [squash]" } },
  { name: "agent-name branch", overrides: { head: { ref: "agent/x99-codex-luna-2/1a2b3c4d" } } },
  { name: "non-CHE Part of directive", overrides: { body: "Part of MUL-5" } },
]) {
  test(`a ${name} does not supply a CHE issue link`, () => {
    const result = run(overrides);
    assert.equal(result.status, 1, result.stdout + result.stderr);
    assert.match(result.stdout, /PR has no issue link/);
  });
}

test("a CHE Part of directive supplies a link beside a utf-8 title", () => {
  const result = run({
    title: "fix: utf-8 decoding [squash]",
    body: "Part of CHE-2389",
  });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /reference.*CHE-2389/i);
});

test("a Part of-only PR passes without declaring completion", () => {
  const result = run({ body: "Part of CHE-2387" });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /reference.*CHE-2387/i);
  assert.match(result.stdout, /effective target\(s\): none/);
});

test("body reference and close directives supply links", () => {
  for (const directive of ["Refs", "Ref:", "Part of", "part\tof:", "Closes", "Fixes", "Resolves"]) {
    const result = run({ body: `${directive} CHE-2389` });
    assert.equal(result.status, 0, `${directive}: ${result.stdout}${result.stderr}`);
  }
});

test("title and branch keys supply links", () => {
  for (const overrides of [
    { title: "ci: require issue links (CHE-2389) [squash]" },
    { head: { ref: "fix/che-2389-checker" } },
  ]) {
    const result = run(overrides);
    assert.equal(result.status, 0, result.stdout + result.stderr);
  }
});

test("code-only directives and bare body keys do not supply links", () => {
  for (const body of [
    "`Part of CHE-2387`",
    "``Refs CHE-2387` example``",
    "```text\nCloses CHE-2389\n```",
    "~~~\nPart of CHE-2387\n~~~",
    "```\nRefs CHE-2387",
    "Discuss CHE-2389 without linking the issue.",
    "Part of the feature CHE-2387",
  ]) {
    const result = run({ body });
    assert.equal(result.status, 1, `${body}: ${result.stdout}${result.stderr}`);
  }
});

test("upstream-sync heads and dependabot authors are exempt", () => {
  for (const overrides of [
    { head: { ref: "upstream-sync/main/abc123" } },
    { user: { login: "dependabot[bot]" } },
  ]) {
    const result = run(overrides);
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.match(result.stdout, /skipped/);
  }
});

test("a dependabot branch alone does not exempt a human PR", () => {
  assert.equal(run({ head: { ref: "dependabot/npm/example" } }).status, 1);
});

test("Part of does not hide a conflicting completion target", () => {
  const result = run({
    title: "fix: child (CHE-2389) [squash]",
    head: { ref: "fix/che-2387-parent" },
    body: "Closes CHE-2389\nPart of CHE-2387",
  });
  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stdout, /CHE-2387.*branch/);
});

test("the workflow uses trusted checker code with read-only credentials", () => {
  const workflow = readFileSync(new URL("../.github/workflows/pr-close-target-check.yml", import.meta.url), "utf8");
  assert.match(workflow, /pull_request_target:/);
  assert.match(workflow, /ref: \$\{\{ github\.workflow_sha \}\}/);
  assert.match(workflow, /persist-credentials: false/);
  assert.match(workflow, /contents: read/);
  assert.doesNotMatch(workflow, /ref:.*pull_request\.head/);
});
