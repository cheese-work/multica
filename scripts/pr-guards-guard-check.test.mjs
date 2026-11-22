import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
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
const queue = read("pr-guard-rules-queue.yml");

test("pr-guard-rules.yml is a reusable workflow with one job named PR guard rules", () => {
  assert.match(rules, /^on:\n {2}workflow_call:/m);
  assert.match(rules, /^ {2}guard-check:\n {4}name: PR guard rules$/m);
});

test("the check builds the CLI from the trusted workflow commit and never runs the PR head", () => {
  assert.match(rules, /ref: \$\{\{ github\.event\.merge_group\.base_sha \|\| github\.workflow_sha \}\}/);
  assert.match(rules, /persist-credentials: false/);
  assert.match(rules, /go build -o "\$RUNNER_TEMP\/multica" \.\/cmd\/multica/);
  assert.doesNotMatch(rules, /pull_request\.head\.(sha|ref)|refs\/pull/);
});

test("the check holds read-only permissions, takes one read-only secret and no PR text in a shell line", () => {
  assert.match(rules, /permissions:\n {6}contents: read\n {6}pull-requests: read/);
  assert.doesNotMatch(rules, /: write|continue-on-error|pull_request\.(title|body)/);
  assert.deepEqual([...rules.matchAll(/secrets\.(\w+)/g)].map((m) => m[1]), ["GUARD_POLICY_READ_TOKEN"]);
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

for (const [label, wf] of [["pr-guards.yml", guards], ["pr-guard-rules-edit.yml", edits], ["pr-guard-rules-queue.yml", queue]]) {
  test(`${label} calls the reusable check as job guard-check with read-only permissions, skipping closed PRs`, () => {
    const job = callerJob(wf);
    assert.match(job, /uses: \.\/\.github\/workflows\/pr-guard-rules\.yml/);
    assert.match(job, /permissions:\n {6}contents: read\n {6}pull-requests: read/);
    if (wf !== queue) assert.match(job, /if: github\.event\.pull_request\.state != 'closed'/);
    assert.match(job, /secrets:\n {6}GUARD_POLICY_READ_TOKEN: \$\{\{ secrets\.GUARD_POLICY_READ_TOKEN \}\}\n?$/);
  });
}

test("a title, body or base edit and a removed reviewer re-run the check, in their own workflow (CHE-4385 budget)", () => {
  assert.match(edits, /^on:\n {2}pull_request_target:\n {4}types: \[edited, review_request_removed\]$/m);
  assert.match(edits, /group: pr-guard-rules-edit-\$\{\{ github\.event\.pull_request\.number \}\}/);
  assert.match(edits, /cancel-in-progress: true/);
  assert.match(guards, /types: \[opened, synchronize, reopened, ready_for_review\]/);
});

// A merge-queue build re-judges the PR at merge time, so a stored-policy, variable or base-rule change since
// the last green PR run cannot let a stale success merge. The queue event runs the merged tree, so the CLI is
// built from the queue's base commit, never from the PR code in it.
test("a merge-queue build runs the same check as job guard-check, with no concurrency cancel", () => {
  assert.match(queue, /^on:\n {2}merge_group:\n {4}types: \[checks_requested\]$/m);
  assert.doesNotMatch(queue, /pull_request/);
  assert.doesNotMatch(queue, /cancel-in-progress/);
});

test("the check builds from the queue's base commit on a merge-queue build", () => {
  assert.match(rules, /ref: \$\{\{ github\.event\.merge_group\.base_sha \|\| github\.workflow_sha \}\}/);
  assert.match(rules, /MERGE_GROUP_HEAD_REF: \$\{\{ github\.event\.merge_group\.head_ref \}\}/);
  assert.doesNotMatch(rules, /merge_group\.head_sha/);
});

// The step logic lives in scripts/pr-guard-rules.sh so it can be run against a fake CLI.
function run(env) {
  const dir = mkdtempSync(join(tmpdir(), "prgr-"));
  const bin = join(dir, "multica");
  const log = join(dir, "calls.log");
  writeFileSync(bin, `#!/bin/sh\nif [ "$1 $2" = "workspace get" ]; then [ -n "$FAKE_WS_FAIL" ] && exit 1; printf '%s' "$FAKE_WS_JSON"; exit 0; fi\necho "args=$* policy=$MULTICA_GUARD_POLICY sha=$MULTICA_GUARD_POLICY_SHA256 gh=$MULTICA_GUARD_REAL_GH body=$(cat "$MULTICA_GUARD_POLICY" 2>/dev/null)" >> "${log}"\nexit "\${FAKE_EXIT:-0}"\n`);
  chmodSync(bin, 0o755);
  const res = spawnSync("bash", [new URL("./pr-guard-rules.sh", import.meta.url).pathname], {
    env: { PATH: process.env.PATH, RUNNER_TEMP: dir, MULTICA_BIN: bin, PR_URL: "https://github.com/o/r/pull/7", REPO: "o/r", ...env },
    encoding: "utf8",
  });
  let calls = "";
  try { calls = readFileSync(log, "utf8"); } catch {}
  return { ...res, calls };
}

// CHE-5934: no stored policy exists, so both variables are unset. The CLI then applies the compiled default policy,
// the same one the daemon's guard uses when no policy is stored (guard.PolicyFromEnv). The check still judges the PR.
test("neither variable set: the CLI runs on the compiled default policy, with no policy env, and a notice says so", () => {
  const r = run({ GUARD_POLICY_JSON: "", GUARD_POLICY_SHA256: "" });
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.stdout, /::notice::.*compiled default/);
  assert.match(r.calls, /args=guard check-pr https:\/\/github\.com\/o\/r\/pull\/7 -R o\/r policy= sha= /);
});

test("neither variable set: a broken rule still fails the check", () => {
  const r = run({ GUARD_POLICY_JSON: "", GUARD_POLICY_SHA256: "", FAKE_EXIT: "1" });
  assert.equal(r.status, 1);
});

test("a digest with no policy: fail closed with exit 2 and never call the CLI", () => {
  const r = run({ GUARD_POLICY_JSON: "", GUARD_POLICY_SHA256: "abc123" });
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

const POLICY = { GUARD_POLICY_JSON: '{"version":1,"plugins":[]}', GUARD_POLICY_SHA256: "abc123" };

test("merge-queue build: the PR number comes from the queue branch name", () => {
  for (const ref of [
    "refs/heads/gh-readonly-queue/main/pr-1560-0123456789abcdef0123456789abcdef01234567",
    "refs/heads/gh-readonly-queue/release/2026/pr-7-0123456789abcdef0123456789abcdef01234567",
  ]) {
    const n = ref.match(/pr-(\d+)-/)[1];
    const r = run({ ...POLICY, PR_URL: "", MERGE_GROUP_HEAD_REF: ref });
    assert.equal(r.status, 0, r.stderr);
    assert.match(r.calls, new RegExp(`args=guard check-pr https://github\\.com/o/r/pull/${n} -R o/r `));
  }
});

test("merge-queue build: an unreadable queue branch name fails closed with exit 2", () => {
  for (const ref of ["", "refs/heads/main", "refs/heads/gh-readonly-queue/main/pr-x-abc", "refs/heads/gh-readonly-queue/main/pr-12-abc;id"]) {
    const r = run({ ...POLICY, PR_URL: "", MERGE_GROUP_HEAD_REF: ref });
    assert.equal(r.status, 2, ref);
    assert.match(r.stdout + r.stderr, /::error::/);
    assert.equal(r.calls, "", ref);
  }
});

test("a PR event keeps using its own PR URL, whatever the queue variable holds", () => {
  const r = run({ ...POLICY, MERGE_GROUP_HEAD_REF: "refs/heads/gh-readonly-queue/main/pr-9-0123456789abcdef0123456789abcdef01234567" });
  assert.match(r.calls, /pull\/7 -R o\/r /);
});

// CHE-5752: the variables are a copy of the stored policy. With a read token the check reads the live stored policy
// (workspace settings.guard_policy) and fails closed when the copy differs or freshness cannot be established.
const POL = '{"version":1,"plugins":[],"review":[{"repo":"o/r","reviewers":["a"]}]}';
const live = (policy) => JSON.stringify({ settings: policy === null ? {} : { guard_policy: JSON.parse(policy) } });
const withToken = { GUARD_POLICY_READ_TOKEN: "mul_t", MULTICA_SERVER_URL: "https://c00.example", MULTICA_WORKSPACE_ID: "ws1" };

test("live policy equals the variable copy (key order aside): the CLI runs", () => {
  const reordered = '{"review":[{"reviewers":["a"],"repo":"o/r"}],"plugins":[],"version":1}';
  const r = run({ ...withToken, GUARD_POLICY_JSON: reordered, GUARD_POLICY_SHA256: "abc123", FAKE_WS_JSON: live(POL) });
  assert.equal(r.status, 0, r.stdout + r.stderr);
  assert.match(r.calls, /args=guard check-pr /);
});

test("a stale but matching JSON/SHA pair fails: the live policy gained a reviewer, the CLI never runs", () => {
  const newer = '{"version":1,"plugins":[],"review":[{"repo":"o/r","reviewers":["a","b"]}]}';
  const r = run({ ...withToken, GUARD_POLICY_JSON: POL, GUARD_POLICY_SHA256: "abc123", FAKE_WS_JSON: live(newer) });
  assert.equal(r.status, 2);
  assert.match(r.stdout + r.stderr, /::error::.*stale/);
  assert.equal(r.calls, "");
});

test("the live policy is set but the variables are unset: fail closed", () => {
  const r = run({ ...withToken, GUARD_POLICY_JSON: "", GUARD_POLICY_SHA256: "", FAKE_WS_JSON: live(POL) });
  assert.equal(r.status, 2);
  assert.match(r.stdout + r.stderr, /::error::.*stale/);
  assert.equal(r.calls, "");
});

test("no stored policy live and variables unset: the CLI runs on the compiled default", () => {
  const r = run({ ...withToken, GUARD_POLICY_JSON: "", GUARD_POLICY_SHA256: "", FAKE_WS_JSON: live(null) });
  assert.equal(r.status, 0, r.stdout + r.stderr);
});

test("the live policy cannot be read: fail closed", () => {
  const r = run({ ...withToken, GUARD_POLICY_JSON: POL, GUARD_POLICY_SHA256: "abc123", FAKE_WS_FAIL: "1" });
  assert.equal(r.status, 2);
  assert.match(r.stdout + r.stderr, /::error::.*live/);
  assert.equal(r.calls, "");
});

test("live check required but no read token: fail closed", () => {
  const r = run({ GUARD_POLICY_LIVE_REQUIRED: "1", GUARD_POLICY_JSON: POL, GUARD_POLICY_SHA256: "abc123" });
  assert.equal(r.status, 2);
  assert.match(r.stdout + r.stderr, /::error::.*GUARD_POLICY_READ_TOKEN/);
  assert.equal(r.calls, "");
});

test("no read token and not required: unchanged behavior, with a notice that freshness is unchecked", () => {
  const r = run({ GUARD_POLICY_JSON: POL, GUARD_POLICY_SHA256: "abc123" });
  assert.equal(r.status, 0);
  assert.match(r.stdout, /::notice::.*not checked/);
});

for (const body of ["null", "{}", '{"settings":null}', '{"settings":[]}', '"x"']) {
  test(`a workspace response with no settings object (${body}) is not a read: fail closed, the CLI never runs`, () => {
    const r = run({ ...withToken, GUARD_POLICY_JSON: "", GUARD_POLICY_SHA256: "", FAKE_WS_JSON: body });
    assert.equal(r.status, 2, r.stdout + r.stderr);
    assert.match(r.stdout + r.stderr, /::error::.*live/);
    assert.equal(r.calls, "");
  });
}

test("a valid empty settings object is a read of no stored policy: the CLI runs", () => {
  const r = run({ ...withToken, GUARD_POLICY_JSON: "", GUARD_POLICY_SHA256: "", FAKE_WS_JSON: '{"settings":{}}' });
  assert.equal(r.status, 0, r.stdout + r.stderr);
  assert.match(r.calls, /args=guard check-pr /);
});

// CHE-8780 (CHE-7066 criterion 8): a merge-group run prints one REST read of the live base tip, with a UTC timestamp,
// next to the CLI's own rules note. The read never changes the result: the CLI exit code is the only decision.
function runQueue(env, { ghExit = 0, ghOut = "" } = {}) {
  const dir = mkdtempSync(join(tmpdir(), "prgr-q-"));
  const bindir = join(dir, "bin");
  mkdirSync(bindir);
  const ghlog = join(dir, "gh.log");
  writeFileSync(join(bindir, "gh"), `#!/bin/sh\necho "$*" >> "${ghlog}"\n[ "${ghExit}" = 0 ] || { echo "HTTP 403: rate limit" >&2; exit ${ghExit}; }\necho "${ghOut}"\n`);
  chmodSync(join(bindir, "gh"), 0o755);
  const r = run({ ...POLICY, PR_URL: "", MERGE_GROUP_HEAD_REF: QREF, GITHUB_EVENT_NAME: "merge_group", PATH: `${bindir}:${process.env.PATH}`, ...env });
  let ghcalls = "";
  try { ghcalls = readFileSync(ghlog, "utf8"); } catch {}
  return { ...r, ghcalls };
}
const QREF = "refs/heads/gh-readonly-queue/release/2026/pr-7-0123456789abcdef0123456789abcdef01234567";
const TIP = "8223552020aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";

test("merge-group run: prints the REST tip of the queue's base with a UTC timestamp", () => {
  const r = runQueue({}, { ghOut: TIP });
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.stdout, new RegExp(`REST tip: ${TIP} \\(release/2026 of o/r, read \\d{4}-\\d\\d-\\d\\dT\\d\\d:\\d\\d:\\d\\dZ\\)`));
  assert.match(r.ghcalls, /api repos\/o\/r\/commits\/heads\/release\/2026 --jq \.sha/);
});

test("merge-group run: a failing gh prints REST tip unavailable and the CLI exit code still decides", () => {
  for (const code of ["0", "1", "2"]) {
    const r = runQueue({ FAKE_EXIT: code }, { ghExit: 1 });
    assert.equal(r.status, Number(code));
    assert.match(r.stdout, /REST tip unavailable: .*rate limit/);
  }
});

test("merge-group run: the REST tip is printed after a failing check too, and the exit code stays the CLI's", () => {
  const r = runQueue({ FAKE_EXIT: "1" }, { ghOut: TIP });
  assert.equal(r.status, 1);
  assert.match(r.stdout, new RegExp(`REST tip: ${TIP}`));
});

test("a PR event makes no REST tip read", () => {
  const r = run({ ...POLICY, GITHUB_EVENT_NAME: "pull_request" });
  assert.equal(r.status, 0);
  assert.doesNotMatch(r.stdout, /REST tip/);
});
