import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// CHE-1676: this repository is public and forkable. A pull_request run takes its workflow
// file from the PR merge commit, so a fork can rewrite any `runs-on` it likes. These
// workflows therefore never name a self-hosted (X99) runner; GitHub-hosted runners are free
// for a public repository.
const read = (name) => readFileSync(new URL(`../.github/workflows/${name}`, import.meta.url), "utf8");
const runsOn = (text) => text.split("\n").filter((line) => /^\s+runs-on:/.test(line));

for (const name of ["ci.yml", "main-build.yml", "pr-guards.yml", "pr-guard-rules.yml"]) {
  test(`${name} runs every job on GitHub-hosted runners`, () => {
    const lines = runsOn(read(name));
    assert.ok(lines.length > 0);
    for (const line of lines) {
      assert.doesNotMatch(line, /cheese-x99|self-hosted/, line.trim());
      assert.match(line, /runs-on: (ubuntu-24\.04|\$\{\{ matrix\.os \}\}|\$\{\{ vars\.RUNNER_[A-Z_]+ \|\| 'ubuntu-24\.04' \}\})\s*(#.*)?$/, line.trim());
    }
  });
}

test("PR guards run trusted code on every PR event they start on, with no `edited` trigger (CHE-4385)", () => {
  const workflow = read("pr-guards.yml");
  assert.match(workflow, /types: \[opened, synchronize, reopened, ready_for_review\]/);
  assert.match(workflow, /ref: \$\{\{ github\.workflow_sha \}\}/);
  assert.doesNotMatch(workflow, /if:.*(action|edited)/);
});

test("ci.yml matrix runners are GitHub-hosted labels", () => {
  assert.doesNotMatch(read("ci.yml"), /^\s+os:.*(cheese-x99|self-hosted)/m);
});

// CHE-2418 B6: X99 runners bind-mount ~/go and ~/.cache from the host's nvme disk, so a Go job
// there already has a warm GOMODCACHE and GOCACHE. setup-go's own cache would download and
// upload the same files through actions/cache. GitHub-hosted runners have no such disk and keep it.
// PR runs never reach X99 (see the tests above), so this local cache is only ever written by
// trusted default-branch and tag runs: a PR cannot plant objects that a release build reads.
test("release.yml leaves the Go cache to the disk on a self-hosted runner", () => {
  const workflow = read("release.yml");
  const verify = workflow.slice(workflow.indexOf("\n  verify:"), workflow.indexOf("\n  release:"));
  assert.match(verify, /cheese-x99/);
  assert.match(verify, /uses: actions\/setup-go@[^\n]+\n\s+with:[\s\S]*?\n\s+cache: \$\{\{ runner\.environment != 'self-hosted' \}\}/);
});

// CHE-3604: the gate is a light job (median 1.5 min), so it runs on a GitHub-hosted runner, free
// for this public repository. CHE-3128 had moved it to a dedicated X99 lane. The trust boundary
// is unchanged: the job runs the default-branch definition.
test("security-gate.yml runs on a GitHub-hosted runner", () => {
  // One hosted job: the gate. mechanical-tier (CHE-4385) is a reusable-workflow call with no runner of its own.
  // The queue-handoff job and security-gate-queue.yml were removed in CHE-9846 (27 of 27 runs skipped).
  const lines = runsOn(read("security-gate.yml"));
  assert.equal(lines.length, 1);
  for (const line of lines) {
    assert.match(line, /runs-on: ubuntu-latest\s*(#.*)?$/);
    assert.doesNotMatch(line, /cheese-x99|self-hosted/);
  }
});

// CHE-7067: dispatch CI runs (agents chain them) shared the 60-job hosted pool; 8 parallel
// runs held 42 slots. One shared group, never cancelled, queues them. PR/merge_group/schedule
// keep the per-PR group with cancel-in-progress (CI standard R2). warning-budget makes ~84
// GitHub API calls per run, so it stays off merge_group (nightly and PR runs still cover it).
test("ci.yml runs one workflow_dispatch CI at a time and keeps cancel for other events (CHE-7067)", () => {
  const ci = read("ci.yml");
  const block = ci.match(/^concurrency:\n((?:[ \t]+.*\n)+)/m)?.[1] ?? "";
  const group = block.match(/group: \$\{\{ (.*) \}\}/)?.[1] ?? "";
  // dispatch branch of the group expression is a constant string
  assert.match(group, /^github\.event_name == 'workflow_dispatch' && '[a-z-]+' \|\| /);
  // every other event keeps the per-PR/ref group
  assert.match(group, /github\.workflow.*github\.event_name.*github\.event\.pull_request\.number \|\| github\.ref/);
  assert.match(block, /cancel-in-progress: \$\{\{ github\.event_name != 'workflow_dispatch' \}\}/);
});

test("warning-budget does not run on merge_group (CHE-7067)", () => {
  const job = read("ci.yml").match(/^  warning-budget:\n(?:(?!^  \S).*\n)+/m)?.[0] ?? "";
  assert.match(job, /^    if: .*github\.event_name != 'merge_group'/m);
});
