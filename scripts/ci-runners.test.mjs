import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// CHE-1676: this repository is public and forkable. A pull_request run takes its workflow
// file from the PR merge commit, so a fork can rewrite any `runs-on` it likes. These
// workflows therefore never name a self-hosted (X99) runner; GitHub-hosted runners are free
// for a public repository. pr-close-target-check.yml is not listed: it is pull_request_target,
// so GitHub runs the base branch's copy and a fork cannot edit it.
const read = (name) => readFileSync(new URL(`../.github/workflows/${name}`, import.meta.url), "utf8");
const runsOn = (text) => text.split("\n").filter((line) => /^\s+runs-on:/.test(line));

for (const name of ["ci.yml", "pr-changelog-check.yml"]) {
  test(`${name} runs every job on GitHub-hosted runners`, () => {
    const lines = runsOn(read(name));
    assert.ok(lines.length > 0);
    for (const line of lines) {
      assert.doesNotMatch(line, /cheese-x99|self-hosted/, line.trim());
      assert.match(line, /runs-on: (ubuntu-24\.04|\$\{\{ matrix\.os \}\})\s*(#.*)?$/, line.trim());
    }
  });
}

test("ci.yml matrix runners are GitHub-hosted labels", () => {
  assert.doesNotMatch(read("ci.yml"), /^\s+os:.*(cheese-x99|self-hosted)/m);
});
