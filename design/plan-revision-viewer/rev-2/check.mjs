import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import vm from "node:vm";

const html = await readFile(new URL("A.html", import.meta.url), "utf8");
assert.doesNotMatch(html, /<(?:script|link)[^>]+(?:src|href)=/);
assert.match(html, /data:font\/ttf;base64,/);
console.log("PASS self-contained HTML embeds its styles, script and font");
const fixture = html.match(/<script id="fixture">([\s\S]*?)<\/script>/)[1];
const context = vm.createContext({});
vm.runInContext(`${fixture}\nthis.preview = { baseLines, currentLines, earlierLines, diffLines, segments };`, context);
const { baseLines, currentLines, earlierLines, diffLines, segments } = context.preview;
assert.ok(baseLines.length >= 300);
assert.ok(baseLines.join("\n").length >= 20000);
assert.ok(currentLines.length >= 300);
for (const [name, base] of [["2", baseLines], ["1", earlierLines]]) {
  const rows = diffLines(base, currentLines);
  assert.deepEqual(Array.from(rows.filter((row) => row.kind !== "add"), (row) => row.text), Array.from(base));
  assert.deepEqual(Array.from(rows.filter((row) => row.kind !== "remove"), (row) => row.text), Array.from(currentLines));
  console.log(`PASS Revision ${name} reconstructs both complete Markdown fixtures`);
}
const rows = diffLines(baseLines, currentLines);
assert.equal(rows.filter((row) => row.kind === "add").length, 5);
assert.equal(rows.filter((row) => row.kind === "remove").length, 2);
const groups = segments(rows);
assert.equal(groups.filter((group) => group.kind === "hunk").length, 3);
assert.ok(groups.some((group) => group.kind === "context" && group.rows.length > 100));
assert.ok(rows.some((row) => row.newNumber >= 300));
assert.equal(segments(diffLines(baseLines, baseLines)).filter((group) => group.kind === "hunk").length, 0);
assert.equal(diffLines(baseLines, []).filter((row) => row.kind === "remove").length, baseLines.length);
console.log(`PASS ${baseLines.length}/${currentLines.length} lines, +5/-2 in 3 hunks, collapsible context, identical/empty states`);
