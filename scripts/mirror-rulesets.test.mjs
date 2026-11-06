import test from "node:test";
import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";

// CHE-3955: ruleset JSON for upstream-main. Force-push and deletion must be
// blocked for every actor (no bypass). Only the c00-multicado App (4093382) may bypass
// the update rule, which is how the mirror job writes.
const dir = new URL("../.github/rulesets/", import.meta.url);
const sets = readdirSync(dir)
  .filter((f) => f.startsWith("upstream-main"))
  .map((f) => JSON.parse(readFileSync(new URL(f, dir), "utf8")));
const types = (s) => s.rules.map((r) => r.type);
const ACTIONS_APP = [{ actor_id: 4093382, actor_type: "Integration", bypass_mode: "always" }];

test("every ruleset targets only refs/heads/upstream-main and is active", () => {
  assert.ok(sets.length >= 1);
  for (const s of sets) {
    assert.equal(s.enforcement, "active");
    assert.deepEqual(s.conditions.ref_name.include, ["refs/heads/upstream-main"]);
  }
});

test("deletion and non_fast_forward are enforced with no bypass actor", () => {
  for (const t of ["deletion", "non_fast_forward"]) {
    const holders = sets.filter((s) => types(s).includes(t));
    assert.ok(holders.length >= 1, `${t} missing`);
    for (const s of holders) assert.deepEqual(s.bypass_actors ?? [], [], `${t} must have no bypass`);
  }
});

test("update rule is bypassed only by the c00-multicado App", () => {
  const holders = sets.filter((s) => types(s).includes("update"));
  assert.ok(holders.length >= 1, "update missing");
  for (const s of holders) assert.deepEqual(s.bypass_actors, ACTIONS_APP);
});
