// CHE-5538 (S4b-SS5), CHE-5539 (S4b-SS6): every raw-element file that ui-drift.md marks captured must have a `cap` call in
// pattern-harness/run.sh and a harness page, so the mark never outruns the capture.
import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import test from "node:test";

const read = (p) => readFileSync(new URL(`../../${p}`, import.meta.url), "utf8");
const drift = read("design/audit/ui-drift.md");
const runSh = read("scripts/design/pattern-harness/run.sh");
const marked = [...drift.matchAll(/^\d+\. `([^`]+)` .* — captured(?: locally)? \(CHE-\d+\)/gm)].map((m) => `packages/views/${m[1]}`);

test("ui-drift.md marks at least the SS5 files captured", () => {
  for (const f of ["agents/components/create-agent-dialog.tsx", "agents/components/inspector/concurrency-picker.tsx",
    "agents/components/inspector/skill-attach.tsx", "agents/components/inspector/visibility-picker.tsx", "agents/components/instructions-editor.tsx"]) {
    assert.ok(marked.includes(`packages/views/${f}`), `${f} not marked captured`);
  }
});

test("each captured file that still exists on main has a cap call with --component", () => {
  for (const f of marked.filter((p) => existsSync(new URL(`../../${p}`, import.meta.url)))) {
    assert.ok(runSh.includes(`--component ${f}`), `${f} is marked captured but run.sh has no cap for it`);
  }
});

test("ui-drift.md marks the SS6 files captured", () => {
  for (const f of ["autopilots/components/add-trigger-dialog.tsx", "common/expandable-description.tsx",
    "editor/html-viewer.tsx", "editor/plan-revision-viewer.tsx", "labels/resource-label-picker.tsx"]) {
    assert.ok(marked.includes(`packages/views/${f}`), `${f} not marked captured`);
  }
});
