import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const read = (path) => readFileSync(new URL(`../${path}`, import.meta.url), "utf8");

// UPSTREAM_VERSION names the upstream Multica release this fork last synced,
// as plain semver on one line. Release stamping reads it, so a sync that moves
// the app packages must move it too.
test("UPSTREAM_VERSION is plain semver and matches the synced upstream release", () => {
  const raw = read("UPSTREAM_VERSION");
  assert.match(raw, /^\d+\.\d+\.\d+\n$/, "one semver line, no prefix or suffix");
  assert.equal(raw.trim(), JSON.parse(read("apps/web/package.json")).version);
});
