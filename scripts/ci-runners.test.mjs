import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// CHE-1676: this repository is public and forkable. A pull_request run takes its workflow
// file from the PR merge commit, so a fork can rewrite any `runs-on` it likes. These
// workflows therefore never name a self-hosted (X99) runner; GitHub-hosted runners are free
// for a public repository.
const read = (name) => readFileSync(new URL(`../.github/workflows/${name}`, import.meta.url), "utf8");
const runsOn = (text) => text.split("\n").filter((line) => /^\s+runs-on:/.test(line));

for (const name of ["ci.yml", "main-build.yml", "pr-changelog-check.yml", "pr-close-target-check.yml"]) {
  test(`${name} runs every job on GitHub-hosted runners`, () => {
    const lines = runsOn(read(name));
    assert.ok(lines.length > 0);
    for (const line of lines) {
      assert.doesNotMatch(line, /cheese-x99|self-hosted/, line.trim());
      assert.match(line, /runs-on: (ubuntu-24\.04|\$\{\{ matrix\.os \}\})\s*(#.*)?$/, line.trim());
    }
  });
}

test("PR close target validates edited events using trusted code without an edited-event skip", () => {
  const workflow = read("pr-close-target-check.yml");
  assert.match(workflow, /types: \[opened, edited, synchronize, reopened\]/);
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
