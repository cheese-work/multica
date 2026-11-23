import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";

import { compareBlocks, lockFileViolations, normalizeText, profileViolations, renderFailure, uncoveredStates } from "./check-fidelity.mjs";

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

// Rule 3 per case (CHE-9088, contract C2). A case may carry `blocks`: the block ids the lock state draws. A mapped block
// that is absent on both sides and not in the list is not judged; a listed block still fails, and a block present on one side fails whatever the list says.
test("rule 3 per case: an unlisted block absent on both sides is not a violation", () => {
  const v = compareBlocks({ ...CASE, blocks: ["a"] }, side([["a", "A"]]), side([["a", "A"]]), ["a", "folded", "title"]);
  assert.deepEqual(v, []);
});

test("rule 3 per case: a listed block absent on both sides still fails, side both", () => {
  const v = compareBlocks({ ...CASE, blocks: ["a", "title"] }, side([["a", "A"]]), side([["a", "A"]]), ["a", "folded", "title"]);
  assert.deepEqual(ids(v), ["missing-block:title"]);
  assert.equal(v[0].side, "both");
});

test("rule 3 per case: a listed block absent on one side still fails", () => {
  const onlyLock = compareBlocks({ ...CASE, blocks: ["a", "b"] }, side([["a", "A"], ["b", "B"]]), side([["a", "A"]]), ["a", "b", "folded"]);
  assert.deepEqual(ids(onlyLock), ["missing-block:b"]);
  assert.equal(onlyLock[0].side, "code");
  const onlyCode = compareBlocks({ ...CASE, blocks: ["a", "b"] }, side([["a", "A"]]), side([["a", "A"], ["b", "B"]]), ["a", "b", "folded"]);
  assert.deepEqual(ids(onlyCode), ["missing-block:b"]);
  assert.equal(onlyCode[0].side, "lock");
});

test("rule 3 per case: an unlisted block present on one side still fails", () => {
  const v = compareBlocks({ ...CASE, blocks: ["a"] }, side([["a", "A"]]), side([["a", "A"], ["x", "X"]]), ["a", "x"]);
  assert.deepEqual(ids(v), ["missing-block:x"]);
  assert.equal(v[0].side, "lock");
});

test("rule 3 per case: a case without `blocks` judges every mapped block, as before", () => {
  const v = compareBlocks(CASE, side([["a", "A"]]), side([["a", "A"]]), ["a", "folded"]);
  assert.deepEqual(ids(v), ["missing-block:folded"]);
  assert.equal(v[0].side, "both");
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

// Lock inventory rules 7, 8, 10, 11 and 12 (CHE-5793 step 3b). Pure functions over what the lock page reports:
// an inventory is { states: [{ state, profile }], wholeFileAnnotation, unclaimed: [{ kind, key }], badAnnotations: [{ why, label }] }.
const FILE = "design/x/approved/lock.html";
const inv = (extra = {}) => ({ states: [{ state: "s1", profile: "card-540" }], wholeFileAnnotation: false, unclaimed: [], badAnnotations: [], ...extra });
const DEVICES = new Map([["card-540", { id: "card-540", width: 540 }], ["phone-358", { id: "phone-358", width: 358 }]]);
const rules = (v) => v.map((x) => x.rule);

test("rules 10, 11 and 12: a clean inventory gives no instance", () => {
  assert.deepEqual(lockFileViolations(FILE, inv()), []);
});

test("unclaimed-lock-content (rule 10): an unmarked text-only state is one instance per item, with file, kind and text", () => {
  const v = lockFileViolations(FILE, inv({ unclaimed: [{ kind: "text", key: "2 · Text only" }, { kind: "text", key: "Just words" }] }));
  assert.deepEqual(rules(v), ["unclaimed-lock-content", "unclaimed-lock-content"]);
  assert.deepEqual({ file: v[1].file, item_kind: v[1].item_kind, item: v[1].item, side: v[1].side }, { file: FILE, item_kind: "text", item: "Just words", side: "lock" });
});

test("unclaimed-lock-content (rule 10): two equal items are two instances (multiset)", () => {
  const same = { kind: "control", key: "Delete" };
  assert.equal(lockFileViolations(FILE, inv({ unclaimed: [same, same] })).length, 2);
});

test("unclaimed-lock-file (rule 11): a file with no state root that is not an annotation as a whole is one instance for the file", () => {
  const v = lockFileViolations(FILE, inv({ states: [] }));
  assert.deepEqual(rules(v), ["unclaimed-lock-file"]);
  assert.equal(v[0].file, FILE);
});

test("unclaimed-lock-file (rule 11): a file marked data-annotation as a whole is claimed", () => {
  assert.deepEqual(lockFileViolations(FILE, inv({ states: [], wholeFileAnnotation: true })), []);
});

test("bad-marker (rule 12): an annotation that wraps a state root is one instance for the annotation, with its reason", () => {
  const v = lockFileViolations(FILE, inv({ badAnnotations: [{ why: "contains a state root", label: "<div> Notes" }] }));
  assert.deepEqual(rules(v), ["bad-marker"]);
  assert.match(v[0].reason, /contains a state root/);
});

test("bad-marker (rule 12): a state root with no data-profile is one instance for the root", () => {
  const v = lockFileViolations(FILE, inv({ states: [{ state: "s1", profile: null }] }));
  assert.deepEqual(rules(v), ["bad-marker"]);
  assert.equal(v[0].state, "s1");
  assert.match(v[0].reason, /data-profile/);
});

test("bad-marker (rule 12): two roots that share state id and profile are one instance per root", () => {
  const v = lockFileViolations(FILE, inv({ states: [{ state: "s1", profile: "card-540" }, { state: "s1", profile: "card-540" }, { state: "s1", profile: "phone-358" }] }));
  assert.deepEqual(rules(v), ["bad-marker", "bad-marker"]);
  assert.match(v[0].reason, /s1/);
});

test("uncovered-state (rule 7): a lock root with no case is one instance for the root; a root with a case is none", () => {
  const roots = [{ state: "s1", profile: "card-540" }, { state: "s2", profile: "card-540" }];
  const cases = [{ lock_file: FILE, state: "s1", profile: "card-540" }];
  const v = uncoveredStates(FILE, roots, cases);
  assert.deepEqual(v.map((x) => `${x.rule}:${x.state}:${x.profile}`), ["uncovered-state:s2:card-540"]);
  assert.equal(v[0].file, FILE);
});

test("uncovered-state (rule 7): every phone 358 px case dropped is one instance per phone root", () => {
  const roots = [{ state: "s1", profile: "card-540" }, { state: "s21", profile: "phone-358" }, { state: "s22", profile: "phone-358" }];
  const v = uncoveredStates(FILE, roots, [{ lock_file: FILE, state: "s1", profile: "card-540" }]);
  assert.deepEqual(v.map((x) => x.state), ["s21", "s22"]);
});

test("uncovered-state (rule 7): a case in another lock file does not cover the root", () => {
  assert.equal(uncoveredStates(FILE, [{ state: "s1", profile: "card-540" }], [{ lock_file: "other.html", state: "s1", profile: "card-540" }]).length, 1);
});

test("uncovered-state (rule 7): a case with the right state id and another profile does not cover the root (it is also a profile-mismatch, rule 8)", () => {
  const v = uncoveredStates(FILE, [{ state: "s1", profile: "card-540" }], [{ lock_file: FILE, state: "s1", profile: "phone-358" }]);
  assert.deepEqual(v.map((x) => `${x.state}@${x.profile}`), ["s1@card-540"]);
});

test("uncovered-state (rule 7): coverage is per root; one case with an unmatched profile covers no root of that state", () => {
  const roots = [{ state: "s1", profile: "card-540" }, { state: "s1", profile: "phone-358" }];
  const v = uncoveredStates(FILE, roots, [{ lock_file: FILE, state: "s1", profile: "dm-card-520" }]);
  assert.deepEqual(v.map((x) => `${x.state}@${x.profile}`), ["s1@card-540", "s1@phone-358"]);
  const one = uncoveredStates(FILE, roots, [{ lock_file: FILE, state: "s1", profile: "card-540" }]);
  assert.deepEqual(one.map((x) => `${x.state}@${x.profile}`), ["s1@phone-358"]);
});

test("profile-mismatch (rule 8): a profile id that is not in devices.yml", () => {
  const v = profileViolations({ ...CASE, profile: "nope" }, DEVICES, { marked: true, profile: "nope", width: 540 });
  assert.deepEqual(rules(v), ["profile-mismatch"]);
  assert.match(v[0].reason, /not in design\/devices\.yml/);
});

test("profile-mismatch (rule 8): the case's profile differs from the root's data-profile", () => {
  const v = profileViolations({ ...CASE, profile: "phone-358" }, DEVICES, { marked: true, profile: "card-540", width: 358 });
  assert.deepEqual(rules(v), ["profile-mismatch"]);
  assert.match(v[0].reason, /data-profile/);
});

test("profile-mismatch (rule 8): the root's rendered width is more than 1 px off the profile width", () => {
  const off = profileViolations(CASE, DEVICES, { marked: true, profile: "card-540", width: 541.5 });
  assert.deepEqual(rules(off), ["profile-mismatch"]);
  assert.match(off[0].reason, /541\.5/);
  assert.deepEqual(profileViolations(CASE, DEVICES, { marked: true, profile: "card-540", width: 540.9 }), [], "within 1 px");
  assert.deepEqual(profileViolations(CASE, DEVICES, { marked: true, profile: "card-540" }), [], "width not measured");
});

test("profile-mismatch (rule 8): several reasons for one case are still one instance", () => {
  const v = profileViolations({ ...CASE, profile: "phone-358" }, DEVICES, { marked: true, profile: "card-540", width: 540 });
  assert.equal(v.length, 1);
});

test("profile-mismatch (rule 8): a root without marks (numbered-label fallback) is not judged here; rule 12 covers it", () => {
  assert.deepEqual(profileViolations(CASE, DEVICES, { marked: false, profile: null, width: 540 }), []);
});
