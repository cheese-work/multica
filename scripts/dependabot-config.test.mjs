import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import test from "node:test";

// CHE-4680 (CHE-4658 P2, D16 A): static checks on .github/dependabot.yml. A missing file reads as empty so a failing test names it.
const url = new URL("../.github/dependabot.yml", import.meta.url);
const cfg = existsSync(url) ? readFileSync(url, "utf8") : "";
const blocks = cfg.split(/^ {2}- package-ecosystem: /m).slice(1);
const ecosystems = ["gomod", "npm", "github-actions", "docker"];

test("covers gomod, npm, github-actions and docker, nothing else", () => {
  assert.deepEqual(blocks.map((b) => b.split("\n")[0].replaceAll('"', "")).sort(), [...ecosystems].sort());
});

for (const eco of ecosystems) {
  const block = () => blocks.find((b) => b.startsWith(eco)) ?? "";
  test(`${eco}: Monday weekly in Asia/Ho_Chi_Minh, limit 3`, () => {
    assert.match(block(), /interval: "weekly"/);
    assert.match(block(), /day: "monday"/);
    assert.match(block(), /timezone: "Asia\/Ho_Chi_Minh"/);
    assert.match(block(), /open-pull-requests-limit: 3\b/);
  });
  test(`${eco}: one version-updates group and one security-updates group, each matching "*"`, () => {
    assert.equal((block().match(/applies-to: version-updates/g) ?? []).length, 1);
    assert.equal((block().match(/applies-to: security-updates/g) ?? []).length, 1);
    assert.equal((block().match(/- "\*"/g) ?? []).length, 2);
  });
  test(`${eco}: title is a Conventional Commit with a scope, not feat/fix/perf`, () => {
    assert.match(block(), /prefix: "(build|ci)"/);
    assert.match(block(), /include: scope/);
  });
}

test("gomod targets server/, npm targets the workspace root", () => {
  assert.match(blocks.find((b) => b.startsWith("gomod")) ?? "", /directory: "\/server"/);
  assert.match(blocks.find((b) => b.startsWith("npm")) ?? "", /directory: "\/"/);
});
