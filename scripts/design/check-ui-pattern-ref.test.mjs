import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import test from "node:test";

import { checkUiPatternRef } from "./check-ui-pattern-ref.mjs";

// Each test builds a throwaway git repo: `base` is the PR base, `head` the PR head ("fixture PR").
const LEDGER = `- id: selection-annotation
  intent: Quote selected text.
  origin: upstream
  status: active
  components: []
  frame: {state: pending}
`;
const VIEW = "packages/views/issues/components/thing.tsx";
const withButton = (extra = "") => `export const Thing = () => (\n  <div>\n${extra}    <button type="button">Go</button>\n  </div>\n);\n`;
const plain = "export const Thing = () => <div>hi</div>;\n";

function fixture(t, { base, head, remove = [], rename = [] }) {
  const cwd = mkdtempSync(join(tmpdir(), "ui-pattern-ref-"));
  t.after(() => rmSync(cwd, { recursive: true, force: true }));
  const git = (...args) => execFileSync("git", args, { cwd, encoding: "utf8" });
  const put = (files) => {
    for (const [path, text] of Object.entries(files)) {
      mkdirSync(dirname(join(cwd, path)), { recursive: true });
      writeFileSync(join(cwd, path), text);
    }
  };
  git("init", "-q", "-b", "main");
  git("config", "user.email", "t@example.com");
  git("config", "user.name", "t");
  put({ "design/patterns.yml": LEDGER, ...base });
  git("add", "-A");
  git("commit", "-q", "-m", "base");
  const baseSha = git("rev-parse", "HEAD").trim();
  for (const [from, to] of rename) {
    mkdirSync(dirname(join(cwd, to)), { recursive: true });
    git("mv", from, to);
  }
  for (const path of remove) git("rm", "-q", path);
  put(head);
  git("add", "-A");
  git("commit", "-q", "-m", "head");
  return { cwd, baseSha };
}

const run = (t, spec, body = "") => {
  const { cwd, baseSha } = fixture(t, spec);
  return checkUiPatternRef({ cwd, base: baseSha, head: "HEAD", body });
};
const errors = (r) => r.errors.join("\n");

// ---- must fail ----

test("fails a UI change with no pattern line and prints the one-line fix", (t) => {
  const r = run(t, { base: { [VIEW]: plain }, head: { [VIEW]: "export const Thing = () => <div>bye</div>;\n" } });
  assert.match(errors(r), /no pattern line/);
  assert.match(errors(r), /Fix: add `pattern: <id>`/);
  assert.match(errors(r), /design\/patterns\.yml/);
});

test("fails a new raw <button> and names the primitive to use", (t) => {
  const r = run(t, { base: { [VIEW]: plain }, head: { [VIEW]: withButton() } }, "pattern: selection-annotation");
  assert.match(errors(r), /packages\/views\/issues\/components\/thing\.tsx:3 raw <button>/);
  assert.match(errors(r), /Fix: use Button from @multica\/ui/);
});

test("fails a multiline raw <button> once", (t) => {
  const multi = "export const Thing = () => (\n  <button\n    type=\"button\"\n    onClick={go}\n  >\n    Go\n  </button>\n);\n";
  const r = run(t, { base: { [VIEW]: plain }, head: { [VIEW]: multi } }, "pattern: selection-annotation");
  assert.equal(r.errors.filter((e) => /raw <button>/.test(e)).length, 1);
  assert.match(errors(r), /thing\.tsx:2 raw <button>/);
});

test("fails a pattern line with an unknown id", (t) => {
  const r = run(
    t,
    { base: { [VIEW]: plain }, head: { [VIEW]: "export const Thing = () => <div>bye</div>;\n" } },
    "pattern: no-such-pattern",
  );
  assert.match(errors(r), /unknown pattern id `no-such-pattern`/);
  assert.match(errors(r), /Fix: use an id from design\/patterns\.yml/);
});

test("fails new-pattern and deviation lines without a reason", (t) => {
  const spec = { base: { [VIEW]: plain }, head: { [VIEW]: "export const Thing = () => <div>bye</div>;\n" } };
  assert.match(errors(run(t, spec, "new-pattern: fresh-thing")), /needs a reason/);
  assert.match(errors(run(t, spec, "deviation: selection-annotation")), /needs a reason/);
});

test("fails a baseline that grew", (t) => {
  const baseline = "scripts/design/ui-raw-baseline.json";
  const r = run(t, {
    base: { [VIEW]: withButton(), [baseline]: JSON.stringify({ [VIEW]: 1 }) },
    head: { [baseline]: JSON.stringify({ [VIEW]: 2, "packages/views/other.tsx": 1 }) },
  });
  assert.match(errors(r), /ui-raw-baseline\.json may only shrink/);
});

// ---- must pass ----

test("passes a UI change with a pattern line", (t) => {
  const r = run(
    t,
    { base: { [VIEW]: plain }, head: { [VIEW]: "export const Thing = () => <div>bye</div>;\n" } },
    "## Patterns\n- pattern: selection-annotation\n",
  );
  assert.deepEqual(r.errors, []);
});

test("passes a non-UI change with no pattern line", (t) => {
  const r = run(t, { base: { "scripts/x.mjs": "a\n" }, head: { "scripts/x.mjs": "b\n" } });
  assert.deepEqual(r.errors, []);
  assert.deepEqual(r.uiFiles, []);
});

test("passes new-pattern and deviation lines that carry a reason", (t) => {
  const spec = { base: { [VIEW]: plain }, head: { [VIEW]: "export const Thing = () => <div>bye</div>;\n" } };
  assert.deepEqual(run(t, spec, "new-pattern: fresh-thing — no sibling does this").errors, []);
  assert.deepEqual(run(t, spec, "deviation: selection-annotation — phone width needs a sheet").errors, []);
});

test("passes a renamed file that keeps an old raw element", (t) => {
  const moved = "packages/views/chat/components/thing.tsx";
  const r = run(
    t,
    {
      base: { [VIEW]: withButton() },
      head: {},
      rename: [[VIEW, moved]],
    },
    "pattern: selection-annotation",
  );
  assert.deepEqual(r.errors, []);
  assert.deepEqual(r.uiFiles, [moved]);
});

test("passes a renamed file that gained no new element even when text around it changed", (t) => {
  const moved = "packages/views/chat/components/thing.tsx";
  const r = run(
    t,
    { base: { [VIEW]: withButton() }, head: { [moved]: withButton().replace("hi", "hi") + "export const x = 1;\n" }, rename: [[VIEW, moved]] },
    "pattern: selection-annotation",
  );
  assert.deepEqual(r.errors, []);
});

test("passes a raw element with an inline allow comment and lists it", (t) => {
  const marked = "export const Thing = () => (\n  // ui-pattern-ref: allow native file picker trigger\n  <button type=\"button\">Go</button>\n);\n";
  const r = run(t, { base: { [VIEW]: plain }, head: { [VIEW]: marked } }, "pattern: selection-annotation");
  assert.deepEqual(r.errors, []);
  assert.match(r.allowed.join("\n"), /thing\.tsx:3 .*native file picker trigger/);
});

test("passes a raw element when a deviation line names its file", (t) => {
  const r = run(
    t,
    { base: { [VIEW]: plain }, head: { [VIEW]: withButton() } },
    `deviation: selection-annotation — ${VIEW} keeps a native button for drag handle`,
  );
  assert.deepEqual(r.errors, []);
  assert.match(r.allowed.join("\n"), /deviation/);
});

test("ignores a raw tag with no mapped primitive", (t) => {
  const r = run(
    t,
    { base: { [VIEW]: plain }, head: { [VIEW]: "export const Thing = () => <details><summary>x</summary></details>;\n" } },
    "pattern: selection-annotation",
  );
  assert.deepEqual(r.errors, []);
});

test("ignores raw elements outside packages/views", (t) => {
  const path = "apps/web/app/page.tsx";
  const r = run(t, { base: { [path]: plain }, head: { [path]: withButton() } }, "pattern: selection-annotation");
  assert.deepEqual(r.errors, []);
});

test("fails an edited raw element: its changed lines count as a new element, a baseline count transfers nothing", (t) => {
  const baseline = "scripts/design/ui-raw-baseline.json";
  const reformatted = withButton().replace("<div>", "<div className=\"x\">");
  const r = run(
    t,
    {
      base: { [VIEW]: withButton(), [baseline]: JSON.stringify({ [VIEW]: 1 }) },
      head: { [VIEW]: reformatted.replace("    <button type=\"button\">Go</button>", "    <button type=\"button\">Go!</button>") },
    },
    "pattern: selection-annotation",
  );
  assert.match(errors(r), /thing\.tsx:\d+ raw <button>/);
});

test("a shrunken baseline passes", (t) => {
  const baseline = "scripts/design/ui-raw-baseline.json";
  const r = run(t, {
    base: { [baseline]: JSON.stringify({ [VIEW]: 2, "packages/views/b.tsx": 1 }) },
    head: { [baseline]: JSON.stringify({ [VIEW]: 1 }) },
  });
  assert.deepEqual(r.errors, []);
});

test("fails a raw <input> that replaces a raw <button> in the same file", (t) => {
  const baseline = "scripts/design/ui-raw-baseline.json";
  const r = run(
    t,
    {
      base: { [VIEW]: withButton(), [baseline]: JSON.stringify({ [VIEW]: 1 }) },
      head: { [VIEW]: "export const Thing = () => (\n  <div>\n    <input type=\"text\" />\n  </div>\n);\n" },
    },
    "pattern: selection-annotation",
  );
  assert.match(errors(r), /thing\.tsx:3 raw <input>/);
});

test("fails a second raw <button> added next to an existing one", (t) => {
  const baseline = "scripts/design/ui-raw-baseline.json";
  const r = run(
    t,
    {
      base: { [VIEW]: withButton(), [baseline]: JSON.stringify({ [VIEW]: 1 }) },
      head: { [VIEW]: withButton("    <button type=\"button\">More</button>\n") },
    },
    "pattern: selection-annotation",
  );
  assert.match(errors(r), /raw <button>/);
});

test("fails a baseline that is deleted or invalid on the head", (t) => {
  const baseline = "scripts/design/ui-raw-baseline.json";
  const r = run(t, { base: { [baseline]: JSON.stringify({ [VIEW]: 1 }) }, head: { [baseline]: "not json" } });
  assert.match(errors(r), /ui-raw-baseline\.json may only shrink/);
});

test("fails a raw <button> that replaces a different raw <button> in the same file", (t) => {
  const baseline = "scripts/design/ui-raw-baseline.json";
  const twoButtons = (a, b) =>
    `export const Thing = () => (\n  <div>\n    <button type="button">${a}</button>\n    <p>x</p>\n    <button type="button">${b}</button>\n  </div>\n);\n`;
  const r = run(
    t,
    {
      base: { [VIEW]: twoButtons("Cancel", "Delete"), [baseline]: JSON.stringify({ [VIEW]: 2 }) },
      head: { [VIEW]: twoButtons("Cancel", "Send") },
    },
    "pattern: selection-annotation",
  );
  assert.match(errors(r), /thing\.tsx:5 raw <button>/);
});

test("passes an unchanged raw <button> that only moved lines", (t) => {
  const r = run(
    t,
    { base: { [VIEW]: withButton() }, head: { [VIEW]: withButton("    <p>new text</p>\n") } },
    "pattern: selection-annotation",
  );
  assert.deepEqual(r.errors, []);
});

test("fails a baseline that is valid JSON but not an object of counts", (t) => {
  const baseline = "scripts/design/ui-raw-baseline.json";
  for (const bad of ["false", "[]", JSON.stringify({ [VIEW]: "1" })]) {
    const r = run(t, { base: { [baseline]: JSON.stringify({ [VIEW]: 1 }) }, head: { [baseline]: bad } });
    assert.match(errors(r), /ui-raw-baseline\.json may only shrink/, `head baseline ${bad}`);
  }
});
