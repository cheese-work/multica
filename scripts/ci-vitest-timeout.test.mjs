import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// CHE-2418 B2: the 5 s Vitest default timed out real-git, jsdom and contrast
// tests on loaded runners (package.test.mjs, daemon-localization.test.tsx,
// text-contrast.test.ts); the attempt-2 rerun then passed. packages/views
// already sets 15 s. Hold the other two jsdom suites to the same value.
for (const config of ["apps/desktop/vitest.config.ts", "apps/web/vitest.config.ts"]) {
  test(`${config} sets testTimeout to 15 s`, () => {
    assert.match(readFileSync(new URL(`../${config}`, import.meta.url), "utf8"), /testTimeout:\s*15_000/);
  });
}
