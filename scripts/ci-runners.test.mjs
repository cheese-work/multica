import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const read = (name) => readFileSync(new URL(`../.github/workflows/${name}`, import.meta.url), "utf8");
const runsOn = (text) => text.split("\n").filter((line) => /^\s+runs-on:/.test(line));

// pr-changelog-check.yml stays GitHub-hosted on purpose: fork-changelog.test.mjs pins it.
for (const name of ["ci.yml"]) {
  test(`${name} jobs run on the X99 runners`, () => {
    const lines = runsOn(read(name));
    assert.ok(lines.length > 0);
    for (const line of lines) assert.match(line, /cheese-x99/, line.trim());
  });

  test(`${name} keeps upstream-sync heads off X99`, () => {
    for (const line of runsOn(read(name)).filter((l) => l.includes("ubuntu-latest"))) {
      assert.match(line, /upstream-sync\//, line.trim());
    }
  });
}
