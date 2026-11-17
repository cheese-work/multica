import assert from "node:assert/strict";
import test from "node:test";
import { checkMigrationPrefixes } from "./migration-prefix-check.mjs";
import { checkOrder } from "./migration-order-check.mjs";

// Tree as fork main is at the pin: the lexically last unmapped migration is 999j_.
const unmapped = ["001_init", "99950_issue_view_preference_sidebar_projects", "9999918_skill_version_index", "999j_weekly_run_guard_refusal_action_idx"];
const file = (stem) => `${stem}.up.sql`;
const tree = (mapped) => ({
  upFiles: [...unmapped, ...mapped.map((m) => m.to)].map(file),
  downFiles: [...unmapped, ...mapped.map((m) => m.to)].map((stem) => `${stem}.down.sql`),
  registryKeys: [...unmapped],
});
const good = [
  { from: "618_a", to: "999k_a" },
  { from: "619_b", to: "999l_b" },
];

test("a map after the last unmapped stem, in order, with down files passes", () => {
  assert.deepEqual(checkOrder({ map: good, ...tree(good) }), []);
});

// The numeric suggestion of migration-prefix-check.mjs (max prefix + 1) sorts before 999j_.
test("a stem from the numeric suggestion of migration-prefix-check fails", () => {
  const { nextFree } = checkMigrationPrefixes({ files: [], main: unmapped.map((stem) => `server/migrations/${stem}.up.sql`).filter((p) => /\/\d+_/.test(p)), pulls: [] });
  const map = [{ from: "618_a", to: `${nextFree}_a` }];
  const errors = checkOrder({ map, ...tree(map) });
  assert.equal(errors.length, 1);
  assert.match(errors[0], new RegExp(`${nextFree}_a.*does not sort after 999j_`));
});

test("mapped stems out of upstream order fail", () => {
  const map = [good[1], good[0]];
  assert.match(checkOrder({ map, ...tree(map) }).join("\n"), /out of order/);
});

test("a mapped stem without a down file fails", () => {
  const t = tree(good);
  t.downFiles = t.downFiles.filter((f) => f !== "999l_b.down.sql");
  assert.match(checkOrder({ map: good, ...t }).join("\n"), /999l_b.*no down file/);
});

test("a registry key that names no existing stem fails", () => {
  const t = tree(good);
  t.registryKeys = [...t.registryKeys, "618_a"];
  assert.match(checkOrder({ map: good, ...t }).join("\n"), /618_a.*no existing migration/);
});

// Upstream order is the byte order of the source stems (the upstream runner sorts file names too).
test("source stems in reverse upstream order fail even when the target stems ascend", () => {
  const map = [{ from: "619_b", to: "999k_b" }, { from: "618_a", to: "999l_a" }];
  assert.match(checkOrder({ map, ...tree(map) }).join("\n"), /618_a.*source.*out of upstream order/);
});

test("a duplicate source stem fails", () => {
  const map = [{ from: "618_a", to: "999k_a" }, { from: "618_a", to: "999l_a" }];
  assert.match(checkOrder({ map, ...tree(map) }).join("\n"), /618_a.*source.*out of upstream order/);
});

test("a missing from or to stem fails", () => {
  const map = [{ from: "618_a" }];
  assert.match(checkOrder({ map, ...tree([]) }).join("\n"), /entry 0 needs string from and to/);
});
