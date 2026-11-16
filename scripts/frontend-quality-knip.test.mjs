import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// CHE-6039: the v0.9.0 sync merges dropped the fork's advisory knip step, so
// existing knip findings (squirrel, e2e esbuild/postcss) failed frontend-build.
const action = readFileSync(
  new URL("../.github/actions/frontend-quality/action.yml", import.meta.url),
  "utf8",
);
const knipStep =
  action.match(/- name: Check for dead code and phantom dependencies \(knip\)\n([\s\S]*?)(?=\n    - name:|$)/)?.[1] ?? "";

test("the knip step stays advisory in both callers", () => {
  assert.ok(knipStep, "knip step exists");
  assert.match(knipStep, /^\s*continue-on-error: true$/m);
  assert.match(knipStep, /run: pnpm knip/);
});
