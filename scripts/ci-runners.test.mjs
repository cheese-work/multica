import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// CHE-1676: this repository is public and forkable. A pull_request run takes its workflow
// file from the PR merge commit, so a fork can rewrite any `runs-on` it likes. These
// workflows therefore never name a self-hosted (X99) runner; GitHub-hosted runners are free
// for a public repository.
const read = (name) => readFileSync(new URL(`../.github/workflows/${name}`, import.meta.url), "utf8");
const runsOn = (text) => text.split("\n").filter((line) => /^\s+runs-on:/.test(line));

for (const name of ["ci.yml", "pr-changelog-check.yml", "pr-close-target-check.yml"]) {
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
