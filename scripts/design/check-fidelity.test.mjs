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

test("missing-block: a mapped block absent on both sides is one instance, side both", () => {
  const v = compareBlocks(CASE, side([]), side([]), ["a", "b"]);
  assert.deepEqual(ids(v), ["missing-block:a", "missing-block:b"]);
  assert.ok(v.every((x) => x.side === "both"));
});

// Rules 1, 2 and 4 (CHE-5793 step 3). A side may also carry `unmapped` (items outside every mapped block, each
// { kind, key }) and `dups` (block id -> element count, for a non-repeat block that matches more than one element).
const withExtra = (s, extra) => ({ ...s, ...extra });
const A = side([["a", "Alpha"]]);

test("unmapped-lock-content (rule 1): a lock item outside every mapped block is one instance with kind and text", () => {
  const v = compareBlocks(CASE, withExtra(A, { unmapped: [{ kind: "text", key: "Stray lock note" }] }), A);
  assert.equal(v.length, 1);
  assert.equal(v[0].rule, "unmapped-lock-content");
  assert.equal(v[0].side, "lock");
  assert.equal(v[0].item_kind, "text");
  assert.equal(v[0].item, "Stray lock note");
  assert.match(v[0].reason, /Stray lock note/);
});

test("unmapped-code-content (rule 2): a code item outside every mapped block is one instance, side code", () => {
  const v = compareBlocks(CASE, A, withExtra(A, { unmapped: [{ kind: "control", key: "Delete" }] }));
  assert.equal(v.length, 1);
  assert.equal(v[0].rule, "unmapped-code-content");
  assert.equal(v[0].side, "code");
  assert.equal(v[0].item_kind, "control");
  assert.equal(v[0].item, "Delete");
});

test("rules 1 and 2: two equal unmapped items are two instances (multiset), not one", () => {
  const two = [{ kind: "text", key: "Same" }, { kind: "text", key: "Same" }];
  assert.equal(compareBlocks(CASE, withExtra(A, { unmapped: two }), A).length, 2);
  assert.equal(compareBlocks(CASE, A, withExtra(A, { unmapped: two })).length, 2);
});

test("rules 1 and 2: the same text as a text node and as a control are different instances (kind is part of the identity)", () => {
  const v = compareBlocks(CASE, withExtra(A, { unmapped: [{ kind: "text", key: "Go" }, { kind: "control", key: "Go" }] }), A);
  assert.deepEqual(v.map((x) => x.item_kind).sort(), ["control", "text"]);
});

test("rule 1 does not fire for an item the code also lacks: each side is judged on its own", () => {
  const v = compareBlocks(CASE, withExtra(A, { unmapped: [{ kind: "text", key: "x" }] }), withExtra(A, { unmapped: [{ kind: "text", key: "x" }] }));
  assert.deepEqual(v.map((x) => x.rule).sort(), ["unmapped-code-content", "unmapped-lock-content"]);
});

test("duplicate-block (rule 4): a block id that matches two lock elements is one instance for that block, side lock", () => {
  const v = compareBlocks(CASE, withExtra(A, { dups: { a: 2 } }), A);
  assert.equal(v.length, 1);
  assert.equal(v[0].rule, "duplicate-block");
  assert.equal(v[0].block, "a");
  assert.equal(v[0].side, "lock");
  assert.equal(v[0].count, 2);
});

test("duplicate-block (rule 4): a duplicate on the code side, and on both sides, is one instance per side", () => {
  assert.deepEqual(compareBlocks(CASE, A, withExtra(A, { dups: { a: 3 } })).map((x) => `${x.rule}:${x.block}:${x.side}`), ["duplicate-block:a:code"]);
  const both = compareBlocks(CASE, withExtra(A, { dups: { a: 2 } }), withExtra(A, { dups: { a: 2 } }));
  assert.deepEqual(both.map((x) => x.side).sort(), ["code", "lock"]);
});

test("sides without unmapped or dups give no rule 1, 2 or 4 instance", () => {
  assert.deepEqual(compareBlocks(CASE, A, A), []);
});
