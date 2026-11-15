import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import {
  DEVICES_PATH,
  BLOCKS_PATH,
  validateDevices,
  validateBlocksMap,
  validateRepoFiles,
} from "./validate-fidelity-files.mjs";

const profile = (over = {}) => ({
  id: "phone-358",
  platform: "web",
  width: 358,
  height: 800,
  dpr: 3,
  themes: ["dark"],
  font_pack: "inter-dejavu-noto",
  ...over,
});
const devices = (profiles = [profile()]) => ({ profiles });

const LOCK = "design/decision-card/approved/direction-b.html";
const kase = (over = {}) => ({ lock_file: LOCK, state: "s1", profile: "phone-358", fixture: "run-budget-top-up", ...over });
const map = (over = {}) => ({
  topic: "decision-card",
  cases: [kase()],
  blocks: { "decision-request-ask": { selector: ".card .ask" } },
  ...over,
});
const ctx = { profileIds: new Set(["phone-358"]), lockLabels: new Set(["1"]) };

test("repo files exist and pass (C1, C2)", () => {
  const r = validateRepoFiles({ read: (p) => readFileSync(p, "utf8") });
  assert.deepEqual(r.errors, []);
});
test("paths are the contract paths", () => {
  assert.equal(DEVICES_PATH, "design/devices.yml");
  assert.equal(BLOCKS_PATH, "tests/fidelity/decision-card.blocks.yml");
});

test("devices: accepts a complete profile", () => {
  assert.deepEqual(validateDevices(devices()).errors, []);
});
test("devices: every field is required", () => {
  for (const k of ["id", "platform", "width", "height", "dpr", "themes", "font_pack"]) {
    const p = profile();
    delete p[k];
    assert.match(validateDevices(devices([p])).errors.join("\n"), new RegExp(k), k);
  }
});
test("devices: rejects bad platform, size, duplicate id, empty list", () => {
  assert.match(validateDevices(devices([profile({ platform: "palm" })])).errors.join(), /platform/);
  assert.match(validateDevices(devices([profile({ width: 0 })])).errors.join(), /width/);
  assert.match(validateDevices(devices([profile({ dpr: -1 })])).errors.join(), /dpr/);
  assert.match(validateDevices(devices([profile({ themes: [] })])).errors.join(), /themes/);
  assert.match(validateDevices(devices([profile(), profile()])).errors.join(), /duplicate/);
  assert.match(validateDevices(devices([])).errors.join(), /at least one/);
});

test("blocks: accepts a complete map", () => {
  assert.deepEqual(validateBlocksMap(map(), ctx).errors, []);
});
test("blocks: case needs lock file, state, profile, fixture", () => {
  for (const k of ["lock_file", "state", "profile", "fixture"]) {
    const c = kase();
    delete c[k];
    assert.match(validateBlocksMap(map({ cases: [c] }), ctx).errors.join(), new RegExp(k), k);
  }
});
test("blocks: unknown profile, unknown state label, duplicate state x profile", () => {
  assert.match(validateBlocksMap(map({ cases: [kase({ profile: "nope" })] }), ctx).errors.join(), /profile nope/);
  assert.match(validateBlocksMap(map({ cases: [kase({ state: "s99" })] }), ctx).errors.join(), /s99/);
  assert.match(validateBlocksMap(map({ cases: [kase(), kase()] }), ctx).errors.join(), /duplicate/);
});
test("blocks: block id is kebab-case; a block the code does not tag yet is allowed (check-fidelity reports it)", () => {
  const m = map({ blocks: { "decision-card.facts": { selector: ".facts" } } });
  assert.match(validateBlocksMap(m, ctx).errors.join(), /decision-card\.facts.*kebab-case/);
  assert.deepEqual(validateBlocksMap(map({ blocks: { "decision-request-title": { selector: ".hd b" } } }), ctx).errors, []);
});
test("blocks: annotation field is not allowed; repeat must be boolean", () => {
  assert.match(validateBlocksMap(map({ annotations: [] }), ctx).errors.join(), /annotation/);
  const m = map({ blocks: { "decision-request-ask": { selector: ".ask", annotation: true } } });
  assert.match(validateBlocksMap(m, ctx).errors.join(), /annotation/);
  const r = map({ blocks: { "decision-request-ask": { selector: ".ask", repeat: "yes" } } });
  assert.match(validateBlocksMap(r, ctx).errors.join(), /repeat/);
});

test("a case may carry interaction steps; a malformed step is an error", () => {
  const ok = [{ click: "text=Details" }, { type: "[role=textbox]", text: "hi" }];
  assert.deepEqual(validateBlocksMap(map({ cases: [kase({ steps: ok })] }), ctx).errors, []);
  for (const bad of [{ click: 1 }, { type: "x" }, { drag: "x" }, "click"]) {
    assert.match(validateBlocksMap(map({ cases: [kase({ steps: [bad] })] }), ctx).errors.join(), /steps\[0\]/, JSON.stringify(bad));
  }
  assert.match(validateBlocksMap(map({ cases: [kase({ steps: "x" })] }), ctx).errors.join(), /steps must be a list/);
});
