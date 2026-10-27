import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { runInNewContext } from "node:vm";

const source = await readFile(new URL("./preview.js", import.meta.url), "utf8");
for (const base of ["1", "2", "2a", "2b"]) {
  const fixture = runInNewContext(
    `${source.replace(/^render\(\);$/m, "")}\n({ rows: fixtureChanges(), currentText, previousText, firstText })`,
    { URLSearchParams, location: { search: `?base=${base}` }, document: { body: { dataset: { direction: "A" } } } },
  );
  const rows = Array.from(fixture.rows);
  const before = rows.filter((row) => row.kind !== "add").map((row) => row.text);
  const after = rows.filter((row) => row.kind !== "remove").map((row) => row.text);
  assert.deepEqual(before, Array.from(base === "1" ? fixture.firstText : fixture.previousText));
  assert.deepEqual(after, Array.from(fixture.currentText));
  assert.equal(rows.filter((row) => row.kind === "remove").length, 2);
  assert.equal(rows.filter((row) => row.kind === "add").length, base === "1" ? 8 : 5);
  console.log(`PASS design fixture base ${base}: before/after reconstruction and line counts`);
}
