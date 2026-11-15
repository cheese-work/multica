import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";

import { compareBlocks, normalizeText, renderFailure } from "./check-fidelity.mjs";

// A side is what one render reports: block id -> { text } for each block found, plus document order.
const side = (entries) => ({ order: entries.map(([id]) => id), blocks: Object.fromEntries(entries.map(([id, text]) => [id, { text }])) });
const CASE = { state: "s1", profile: "card-540", fixture: "run-budget-top-up" };
const ids = (v) => v.map((x) => `${x.rule}:${x.block}`).sort();

test("same blocks, same order, same text: no violation", () => {
  const s = side([["a", "Alpha"], ["b", "Beta"]]);
  assert.deepEqual(compareBlocks(CASE, s, s), []);
});

test("missing-block: a block the lock draws and the code does not render", () => {
  const v = compareBlocks(CASE, side([["a", "Alpha"], ["b", "Beta"]]), side([["a", "Alpha"]]));
  assert.deepEqual(ids(v), ["missing-block:b"]);
  assert.equal(v[0].side, "code");
  assert.deepEqual({ state: v[0].state, profile: v[0].profile }, { state: "s1", profile: "card-540" });
});

test("missing-block: a block the code renders and the lock does not draw", () => {
  const v = compareBlocks(CASE, side([["a", "Alpha"]]), side([["a", "Alpha"], ["b", "Beta"]]));
  assert.deepEqual(ids(v), ["missing-block:b"]);
  assert.equal(v[0].side, "lock");
});

test("order: two swapped blocks are two instances, one per block", () => {
  const v = compareBlocks(CASE, side([["a", "A"], ["b", "B"], ["c", "C"]]), side([["a", "A"], ["c", "C"], ["b", "B"]]));
  assert.deepEqual(ids(v), ["order:b", "order:c"]);
});

test("order: a block missing on one side does not shift the others", () => {
  const v = compareBlocks(CASE, side([["a", "A"], ["x", "X"], ["b", "B"]]), side([["a", "A"], ["b", "B"]]));
  assert.deepEqual(ids(v), ["missing-block:x"]);
});

test("text: a different text is one instance that carries the hash of the code text", () => {
  const v = compareBlocks(CASE, side([["a", "Alpha"]]), side([["a", "Alpha two"]]));
  assert.deepEqual(ids(v), ["text:a"]);
  assert.equal(v[0].code_text_sha256, createHash("sha256").update("Alpha two").digest("hex"));
});

test("text: whitespace and case of layout do not count, words do", () => {
  assert.equal(normalizeText("  Runs \n used  "), "Runs used");
  assert.deepEqual(compareBlocks(CASE, side([["a", "Runs   used"]]), side([["a", "Runs\nused"]])), []);
});

test("one instance per rule x block x case: a missing block is not also a text or order gap", () => {
  const v = compareBlocks(CASE, side([["a", "A"], ["b", "B"]]), side([["a", "Different"]]));
  assert.deepEqual(ids(v), ["missing-block:b", "text:a"]);
});

test("missing-render: a failed render is one instance for the case, with its reason", () => {
  const v = renderFailure(CASE, "code", new Error("fixture not found: run-budget-top-up"));
  assert.equal(v.length, 1);
  assert.equal(v[0].rule, "missing-render");
  assert.equal(v[0].side, "code");
  assert.match(v[0].reason, /fixture not found/);
});

test("every instance names its rule, case and a reason", () => {
  const v = compareBlocks(CASE, side([["a", "A"]]), side([["a", "B"]]));
  for (const x of v) {
    assert.ok(x.rule && x.reason && x.state && x.profile, JSON.stringify(x));
  }
});
