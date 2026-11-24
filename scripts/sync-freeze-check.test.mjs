import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

// CHE-9542: interim merge freeze until PR 1834 (upstream v0.10.0 sync, CHE-6568) merges. While the repository variable
// SYNC_FREEZE is `on`, the PR guard rules check fails every PR except 1834 and PRs labelled `urgent`. The permanent
// rule is CHE-9549. The check runs on pull_request_target, merge_group and the edit workflow, so each is tested.
const read = (name) => readFileSync(new URL(`../.github/workflows/${name}`, import.meta.url), "utf8").replace(/^\s*#.*$/gm, "");
const script = new URL("./sync-freeze-check.sh", import.meta.url).pathname;

// A fake gh answers the label read; FAKE_LABELS is one label per line, FAKE_GH_FAIL makes it fail.
function run(env) {
  const dir = mkdtempSync(join(tmpdir(), "sfz-"));
  const log = join(dir, "gh.log");
  writeFileSync(join(dir, "gh"), `#!/bin/sh\necho "$*" >> "${log}"\n[ -n "$FAKE_GH_FAIL" ] && { echo "boom" >&2; exit 1; }\nprintf '%s' "$FAKE_LABELS"\n`);
  chmodSync(join(dir, "gh"), 0o755);
  const res = spawnSync("bash", [script], {
    env: { PATH: `${dir}:${process.env.PATH}`, REPO: "o/r", ...env },
    encoding: "utf8",
  });
  let calls = "";
  try { calls = readFileSync(log, "utf8"); } catch {}
  return { ...res, calls, out: res.stdout + res.stderr };
}
const pr = (n) => ({ PR_URL: `https://github.com/o/r/pull/${n}`, GITHUB_EVENT_NAME: "pull_request_target" });
const queue = (n) => ({ MERGE_GROUP_HEAD_REF: `refs/heads/gh-readonly-queue/main/pr-${n}-0123456789abcdef0123456789abcdef01234567`, GITHUB_EVENT_NAME: "merge_group" });

for (const [event, make] of [["pull_request_target", pr], ["merge_group", queue]]) {
  test(`${event}: variable off or unset passes any PR and reads nothing`, () => {
    for (const value of ["", "off", "OFF", "false", "0"]) {
      const r = run({ ...make(2000), SYNC_FREEZE: value });
      assert.equal(r.status, 0, `${value}: ${r.out}`);
      assert.equal(r.calls, "");
    }
  });

  test(`${event}: variable on fails a PR with no urgent label`, () => {
    const r = run({ ...make(2000), SYNC_FREEZE: "on", FAKE_LABELS: "bug\nchore\n" });
    assert.equal(r.status, 1, r.out);
    assert.match(r.out, /::error::.*frozen.*1834.*CHE-6568/s);
    assert.match(r.calls, /repos\/o\/r\/issues\/2000\/labels/);
  });

  test(`${event}: variable on, any case and spacing, still freezes`, () => {
    const r = run({ ...make(2000), SYNC_FREEZE: " On ", FAKE_LABELS: "" });
    assert.equal(r.status, 1, r.out);
  });

  test(`${event}: variable on passes PR 1834 without reading labels`, () => {
    const r = run({ ...make(1834), SYNC_FREEZE: "on" });
    assert.equal(r.status, 0, r.out);
    assert.equal(r.calls, "");
  });

  test(`${event}: variable on passes a PR labelled urgent`, () => {
    const r = run({ ...make(2000), SYNC_FREEZE: "on", FAKE_LABELS: "bug\nurgent\n" });
    assert.equal(r.status, 0, r.out);
    assert.match(r.out, /urgent/);
  });

  test(`${event}: a label that only contains urgent does not exempt`, () => {
    const r = run({ ...make(2000), SYNC_FREEZE: "on", FAKE_LABELS: "not-urgent\nurgent-ish\n" });
    assert.equal(r.status, 1, r.out);
  });

  test(`${event}: variable on and the label read fails: fail closed`, () => {
    const r = run({ ...make(2000), SYNC_FREEZE: "on", FAKE_GH_FAIL: "1" });
    assert.equal(r.status, 1, r.out);
    assert.match(r.out, /::error::.*label/);
  });
}

test("variable on and no readable PR number: fail closed", () => {
  const r = run({ SYNC_FREEZE: "on", MERGE_GROUP_HEAD_REF: "refs/heads/gh-readonly-queue/main/weird" });
  assert.equal(r.status, 1, r.out);
  assert.match(r.out, /::error::/);
});

test("variable off and no readable PR number: not judged, passes", () => {
  const r = run({ SYNC_FREEZE: "off", MERGE_GROUP_HEAD_REF: "weird" });
  assert.equal(r.status, 0, r.out);
});

test("PR 1834 is not a prefix match: 18340 and 183 are frozen", () => {
  for (const n of [18340, 183, 11834]) {
    assert.equal(run({ ...pr(n), SYNC_FREEZE: "on", FAKE_LABELS: "" }).status, 1, String(n));
  }
});

// Wiring: the freeze runs inside the one reusable job that pr-guards.yml, the edit workflow and the queue all call.
test("the reusable guard job runs the freeze check from the trusted checkout before the Go build", () => {
  const rules = read("pr-guard-rules.yml");
  assert.match(rules, /run: bash scripts\/sync-freeze-check\.sh/);
  assert.match(rules, /SYNC_FREEZE: \$\{\{ vars\.SYNC_FREEZE \}\}/);
  assert.match(rules, /PR_URL: \$\{\{ github\.event\.pull_request\.html_url \}\}/);
  assert.match(rules, /MERGE_GROUP_HEAD_REF: \$\{\{ github\.event\.merge_group\.head_ref \}\}/);
  assert.ok(rules.indexOf("sync-freeze-check.sh") < rules.indexOf("go build"), "freeze before the Go build");
  assert.ok(rules.indexOf("sync-freeze-check.sh") > rules.indexOf("actions/checkout"), "after the trusted checkout");
});

test("adding or removing the urgent label re-runs the guard so a green or red result follows the label", () => {
  assert.match(read("pr-guard-rules-edit.yml"), /types: \[edited, review_request_removed, labeled, unlabeled\]/);
});
