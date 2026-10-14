import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// CHE-1631 line contract, layer 1 subset: enough to prove the template
// placeholder counts as missing. The full parser lands in layer 2.
const LINE = /^Changelog(\((feat|fix|perf)\))?:\s?(.*)$/;

const root = new URL("../", import.meta.url);
const template = readFileSync(new URL(".github/PULL_REQUEST_TEMPLATE.md", root), "utf8");
const agents = readFileSync(new URL("AGENTS.md", root), "utf8");

const GOOD = [
  "Changelog: Issue lists now open instantly in large workspaces.",
  "Changelog: none",
  "Changelog(fix): Comments that end in an emoji no longer lose their last word.",
];
const BAD = ["Changelog: Refactor X store", "Changelog: CHE-123 add Y"];

// Drops HTML comments (an unclosed one runs to the end) and fenced code.
function liveLines(body) {
  const text = body.replace(/\r\n?/g, "\n").replace(/<!--[\s\S]*?(-->|$)/g, "");
  let fenced = false;
  return text.split("\n").filter((line) => {
    if (/^\s*(```|~~~)/.test(line)) {
      fenced = !fenced;
      return false;
    }
    return !fenced;
  });
}

function changelogValue(body) {
  for (const line of liveLines(body)) {
    const match = LINE.exec(line);
    if (!match) continue;
    const value = match[3].trim();
    return value === "" || /^<.*>$/.test(value) ? null : value;
  }
  return null;
}

test("the template carries one live Changelog line whose placeholder counts as missing", () => {
  const live = liveLines(template).filter((line) => LINE.test(line));
  assert.equal(live.length, 1, "exactly one Changelog line outside comments and fences");
  assert.match(live[0], /^Changelog: <.+>$/);
  assert.equal(changelogValue(template), null, "an unedited template publishes nothing");
  for (const example of GOOD) assert.notEqual(changelogValue(example), null, example);
});

test("the template comment shows the good and bad examples and the hide rule", () => {
  const comment = template.slice(template.indexOf("## Changelog"));
  for (const example of [...GOOD, ...BAD]) assert.ok(comment.includes(example), example);
  assert.match(comment, /without a valid line is hidden/i);
  assert.match(comment, /upstream-sync PRs use `?Changelog: none/i);
});

test("AGENTS.md requires the line with the same examples, verbatim, per stack and for syncs", () => {
  const rule = agents.split("\n").find((line) => line.includes("`Changelog:`")) ?? "";
  assert.match(rule, /every PR/i);
  assert.match(rule, /verbatim/i);
  assert.match(rule, /Closes/);
  assert.match(rule, /lands (a stack )?on `main`/i);
  assert.match(rule, /upstream-sync PRs use `Changelog: none`/i);
  assert.match(rule, /without a valid line is hidden/i);
  for (const example of [...GOOD, ...BAD]) assert.ok(rule.includes(example), example);
});
