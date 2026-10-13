import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const template = readFileSync(new URL("../.github/PULL_REQUEST_TEMPLATE.md", import.meta.url), "utf8");
const instructions = readFileSync(new URL("../AGENTS.md", import.meta.url), "utf8");
const examples = [
  "Changelog: You can see which build is running on the changelog page.",
  "Changelog: none",
  "Changelog(fix): Your task list stays up to date after reconnecting.",
  "Refactor X store",
  "CHE-123 add Y",
];

test("the template's only visible Changelog line counts as missing under the line contract", () => {
  const visible = template.replace(/\r\n/g, "\n").replace(/<!--[\s\S]*?(?:-->|$)/g, "");
  const matches = [...visible.matchAll(/^Changelog(?:\((?:feat|fix|perf)\))?:\s?([^\n]*)$/gm)];
  assert.equal(matches.length, 1);
  const value = matches[0][1].trim();
  const hideReason = !value || /^<[^>]+>$/.test(value) ? "missing" : value === "none" ? "none" : null;
  assert.equal(hideReason, "missing");
  assert.match(value, /^<[^>]+>$/);
  assert.equal(visible.split("\n").filter((line) => line.startsWith("```")).length, 0);
});

test("the template comment explains hidden entries and all five examples", () => {
  const comments = template.match(/<!--[\s\S]*?-->/g) ?? [];
  const guidance = comments.find((comment) => comment.includes("Changelog:"));
  assert.ok(guidance);
  for (const example of examples) assert.ok(guidance.includes(example), example);
  assert.match(guidance, /without a valid line is hidden/);
});

test("the repository PR rule shares examples and preserves the line through PR writing", () => {
  const rule = instructions.split("## Fork Changelog PR Rule")[1];
  assert.ok(rule);
  for (const example of examples) assert.ok(rule.includes(example), example);
  assert.match(rule, /every `cheese-work\/multica` PR/);
  assert.match(rule, /verbatim.*`Closes`.*`Refs`/);
  assert.match(rule, /PR that lands.*stack.*`main`.*whole stack/);
  assert.match(rule, /Upstream-sync PRs use `Changelog: none`/);
});
