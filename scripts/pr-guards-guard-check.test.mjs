import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

// CHE-5208 (T5b of CHE-5194): the blocking backstop for the merge-method tag and the review policy.
// The in-run shim fixes a PR before it is published; this check fails a PR that bypassed the shim
// (absolute path to gh, the web UI, a runtime with no shim). It runs `multica guard check-pr`, built
// from the trusted workflow commit, so it applies the same rules as the shim.
const read = (name) => {
  try {
    return readFileSync(new URL(`../.github/workflows/${name}`, import.meta.url), "utf8").replace(/^\s*#.*$/gm, "");
  } catch {
    return ""; // a missing workflow fails the assertions below, not the module load
  }
};
const rules = read("pr-guard-rules.yml");
const guards = read("pr-guards.yml");
const edits = read("pr-guard-rules-edit.yml");

test("pr-guard-rules.yml is a reusable workflow with one job named PR guard rules", () => {
  assert.match(rules, /^on:\n {2}workflow_call:/m);
  assert.match(rules, /^ {2}guard-check:\n {4}name: PR guard rules$/m);
});

test("the check builds the CLI from the trusted workflow commit and never runs the PR head", () => {
  assert.match(rules, /ref: \$\{\{ github\.workflow_sha \}\}/);
  assert.match(rules, /persist-credentials: false/);
  assert.match(rules, /go build -o "\$RUNNER_TEMP\/multica" \.\/cmd\/multica/);
  assert.doesNotMatch(rules, /pull_request\.head\.(sha|ref)|refs\/pull/);
});

test("the check holds read-only permissions, takes no secret and no PR text in a shell line", () => {
  assert.match(rules, /permissions:\n {6}contents: read\n {6}pull-requests: read/);
  assert.doesNotMatch(rules, /: write|secrets\.|continue-on-error|pull_request\.(title|body)/);
  assert.match(rules, /GH_TOKEN: \$\{\{ github\.token \}\}/);
  assert.match(rules, /PR_URL: \$\{\{ github\.event\.pull_request\.html_url \}\}/);
});

test("the review policy comes from two repository variables, never from the tree", () => {
  assert.match(rules, /GUARD_POLICY_JSON: \$\{\{ vars\.GUARD_POLICY_JSON \}\}/);
  assert.match(rules, /GUARD_POLICY_SHA256: \$\{\{ vars\.GUARD_POLICY_SHA256 \}\}/);
  assert.match(rules, /run: bash scripts\/pr-guard-rules\.sh/);
});

test("the check runs on a GitHub-hosted runner", () => {
  assert.match(rules, /runs-on: \$\{\{ vars\.RUNNER_PR_GUARD_CHECK \|\| 'ubuntu-24\.04' \}\}\s*$/m);
});

const callerJob = (wf) => {
  const m = wf.match(/^ {2}guard-check:\n((?: {4}.*\n)+)/m);
  assert.ok(m, "guard-check caller job");
  return m[1];
};

for (const [label, wf] of [["pr-guards.yml", guards], ["pr-guard-rules-edit.yml", edits]]) {
  test(`${label} calls the reusable check as job guard-check with read-only permissions, skipping closed PRs`, () => {
    const job = callerJob(wf);
    assert.match(job, /uses: \.\/\.github\/workflows\/pr-guard-rules\.yml/);
    assert.match(job, /permissions:\n {6}contents: read\n {6}pull-requests: read/);
    assert.match(job, /if: github\.event\.pull_request\.state != 'closed'/);
    assert.doesNotMatch(job, /secrets:/);
  });
}

test("a title, body or base edit and a removed reviewer re-run the check, in their own workflow (CHE-4385 budget)", () => {
  assert.match(edits, /^on:\n {2}pull_request_target:\n {4}types: \[edited, review_request_removed\]$/m);
  assert.match(edits, /group: pr-guard-rules-edit-\$\{\{ github\.event\.pull_request\.number \}\}/);
  assert.match(edits, /cancel-in-progress: true/);
  assert.match(guards, /types: \[opened, synchronize, reopened, ready_for_review\]/);
});

// The step logic lives in scripts/pr-guard-rules.sh so it can be run against a fake CLI.
function run(env) {
  const dir = mkdtempSync(join(tmpdir(), "prgr-"));
  const bin = join(dir, "multica");
  const log = join(dir, "calls.log");
  writeFileSync(bin, `#!/bin/sh\necho "args=$* policy=$MULTICA_GUARD_POLICY sha=$MULTICA_GUARD_POLICY_SHA256 gh=$MULTICA_GUARD_REAL_GH body=$(cat "$MULTICA_GUARD_POLICY" 2>/dev/null)" >> "${log}"\nexit "\${FAKE_EXIT:-0}"\n`);
  chmodSync(bin, 0o755);
  const res = spawnSync("bash", [new URL("./pr-guard-rules.sh", import.meta.url).pathname], {
    env: { PATH: process.env.PATH, RUNNER_TEMP: dir, MULTICA_BIN: bin, PR_URL: "https://github.com/o/r/pull/7", REPO: "o/r", ...env },
    encoding: "utf8",
  });
  let calls = "";
  try { calls = readFileSync(log, "utf8"); } catch {}
  return { ...res, calls };
}

test("no review policy: fail closed with exit 2 and never call the CLI", () => {
  const r = run({ GUARD_POLICY_JSON: "", GUARD_POLICY_SHA256: "" });
  assert.equal(r.status, 2);
  assert.match(r.stdout + r.stderr, /::error::.*GUARD_POLICY_JSON/);
  assert.equal(r.calls, "");
});

test("a policy with no digest: fail closed with exit 2", () => {
  const r = run({ GUARD_POLICY_JSON: '{"version":1,"plugins":[]}', GUARD_POLICY_SHA256: "" });
  assert.equal(r.status, 2);
  assert.match(r.stdout + r.stderr, /::error::.*GUARD_POLICY_SHA256/);
  assert.equal(r.calls, "");
});

test("policy and digest set: the CLI gets the file, the variable digest (not one computed here) and the PR", () => {
  const r = run({ GUARD_POLICY_JSON: '{"version":1,"plugins":[]}', GUARD_POLICY_SHA256: "abc123" });
  assert.equal(r.status, 0);
  assert.match(r.calls, /args=guard check-pr https:\/\/github\.com\/o\/r\/pull\/7 -R o\/r /);
  assert.match(r.calls, /sha=abc123 /);
  assert.match(r.calls, /body=\{"version":1,"plugins":\[\]\}/);
  assert.match(r.calls, /gh=\S*gh?$|gh=\//m);
});

test("the CLI's exit code (1 broken rule, 2 refused) fails the check", () => {
  for (const code of ["1", "2"]) {
    const r = run({ GUARD_POLICY_JSON: '{"version":1}', GUARD_POLICY_SHA256: "x", FAKE_EXIT: code });
    assert.equal(r.status, Number(code));
  }
});
