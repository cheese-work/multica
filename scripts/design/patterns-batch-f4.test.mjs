import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { resolve } from "node:path";

import { parsePatterns } from "./validate-patterns.mjs";

// CHE-5529 (S4b-F4): each pattern points at its doop frame on the Patterns page.
const BASE = "https://congvc-c00.taila6fa8a.ts.net:34443/c/mRAnnjvGod?frame=";
const EXPECTED = {
  "pause-banner": "leNqyjL3on",
  "run-budget": "RVrN9fCE-8",
  "attachment-card-list": "1mMP-9owTU",
  "wakeup-rule-form": "_wRWCO-9QT",
  "settings-number-row": "aOAZfkP9LP",
};

const ledger = resolve(import.meta.dirname, "../../design/patterns.yml");
const entries = parsePatterns(readFileSync(ledger, "utf8"));

for (const [id, frameId] of Object.entries(EXPECTED)) {
  test(`${id} has a patterns frame`, () => {
    const e = entries.find((x) => x.id === id);
    assert.ok(e, `${id} missing from ledger`);
    assert.deepEqual(e.frame, { state: "patterns", url: BASE + frameId });
  });
}
