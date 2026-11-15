import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import { LOCK_PATH, checkLockMarks, profileIdsOf } from "./check-lock-marks.mjs";

const profiles = profileIdsOf(readFileSync("design/devices.yml", "utf8"));
const page = (body) => `<html><head><style>.a{color:red}</style></head><body>${body}</body></html>`;
const reasons = (html) => checkLockMarks(page(html), profiles).errors.map((e) => e.split(":")[0]);

test("devices.yml profile ids are read", () => {
  assert.deepEqual([...profiles].sort(), ["card-540", "dm-card-520", "phone-358", "web-1440"]);
});
test("the decision-card lock carries every mark (C2)", () => {
  const r = checkLockMarks(readFileSync(LOCK_PATH, "utf8"), profiles);
  assert.deepEqual(r.errors, []);
  assert.equal(r.states.length, 27); // labels 1 to 26 except 22 (annotation), with 1b and 7b
});
test("a marked state root and an annotation pass", () => {
  assert.deepEqual(reasons(`<div class="lbl" data-annotation>1 · Open</div><div data-state="s1" data-profile="card-540"><b>Ask</b></div>`), []);
});
test("text outside every mark fails unclaimed-lock-content", () => {
  assert.deepEqual(reasons(`<p>a note</p>`), ["unclaimed-lock-content"]);
});
test("a state root without a known profile fails bad-marker", () => {
  assert.deepEqual(reasons(`<div data-state="s1" data-profile="nope">x</div>`), ["bad-marker"]);
  assert.deepEqual(reasons(`<div data-state="s1">x</div>`), ["bad-marker"]);
});
test("an annotation that wraps a state root or a control fails bad-marker", () => {
  assert.deepEqual(reasons(`<div data-annotation><div data-state="s1" data-profile="card-540">x</div></div>`), ["bad-marker"]);
  assert.deepEqual(reasons(`<div data-annotation>n <span role="radio">r</span></div>`), ["bad-marker"]);
  assert.deepEqual(reasons(`<div data-annotation>n <button>go</button></div>`), ["bad-marker"]);
});
test("a state label without a state root fails uncovered-state", () => {
  assert.deepEqual(reasons(`<div class="lbl" data-annotation>7 · Batch</div>`), ["uncovered-state"]);
});
test("a control that carries data-annotation itself fails bad-marker", () => {
  assert.deepEqual(reasons(`<button data-annotation>Approve</button>`), ["bad-marker"]);
  assert.deepEqual(reasons(`<input data-annotation>`), ["bad-marker"]);
  assert.deepEqual(reasons(`<span role="button" data-annotation>Revoke</span>`), ["bad-marker"]);
});
