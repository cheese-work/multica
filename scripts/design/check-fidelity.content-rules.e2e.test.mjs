// Rules 1, 2 and 4 on real HTML: a tiny lock, a tiny code page, a tiny blocks map per case, each with one gap.
// Needs Playwright Chromium (no vite build): the code side is a static page that carries [data-fidelity-root].
//   node --test scripts/design/check-fidelity.content-rules.e2e.test.mjs
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

const page = (body) => `<!doctype html><html><body>${body}</body></html>`;
const BLOCKS = { a: { selector: ".a" }, b: { selector: ".b" } };

/** lock: HTML of the state root's inner content; code: HTML of the rendered root's inner content. */
function run({ lock, code, blocks = BLOCKS }) {
  const dir = mkdtempSync(join(tmpdir(), "fidelity-rules-"));
  const lockFile = join(dir, "lock.html");
  const harness = join(dir, "page.html");
  const map = join(dir, "x.blocks.yml");
  writeFileSync(lockFile, page(`<div data-state="s1" data-profile="card-540">${lock}</div>`));
  writeFileSync(harness, page(`<div data-fidelity-root>${code}</div>`));
  const yaml = [
    "topic: fixture", "cases:", `  - lock_file: ${lockFile}`, "    state: s1", "    profile: card-540", "    fixture: none", "blocks:",
    ...Object.entries(blocks).flatMap(([id, b]) => [`  ${id}:`, `    selector: "${b.selector}"`, ...(b.repeat ? ["    repeat: true"] : [])]),
  ].join("\n");
  writeFileSync(map, `${yaml}\n`);
  const out = join(dir, "out");
  const r = spawnSync("node", ["scripts/design/check-fidelity.mjs", "--blocks", map, "--out", out, "--harness", harness], { encoding: "utf8" });
  return { r, result: JSON.parse(readFileSync(join(out, "result.json"), "utf8")) };
}
const of = (result, rule) => result.violations.filter((v) => v.rule === rule);

const CLEAN = `<p class="a">Alpha</p><p class="b">Beta</p>`;
// The code side finds blocks by data-testid, the lock side by the map selector.
const CODE = `<p data-testid="a">Alpha</p><p data-testid="b">Beta</p>`;

test("clean fixture: no violation", () => {
  const { r, result } = run({ lock: CLEAN, code: CODE });
  assert.equal(r.status, 0, r.stderr);
  assert.deepEqual(result.violations, []);
});

test("rule 1 unmapped-lock-content: a lock text outside every block", () => {
  const { r, result } = run({ lock: `${CLEAN}<p class="note">Stray lock note</p>`, code: CODE });
  assert.equal(r.status, 1);
  const v = of(result, "unmapped-lock-content");
  assert.equal(v.length, 1);
  assert.equal(v[0].item_kind, "text");
  assert.equal(v[0].item, "Stray lock note");
});

test("rule 1: lock controls and images outside every block are items too", () => {
  const { result } = run({
    lock: `${CLEAN}<button>Delete</button><img alt="x" src="data:image/gif;base64,R0lGODlhAQABAAAAACw=">`,
    code: CODE,
  });
  const v = of(result, "unmapped-lock-content");
  assert.deepEqual(v.map((x) => x.item_kind).sort(), ["control", "image"]);
  assert.equal(v.find((x) => x.item_kind === "control").item, "Delete");
  assert.match(v.find((x) => x.item_kind === "image").item, /^[0-9a-f]{64}$/);
});

test("rule 2 unmapped-code-content: a code text outside every block", () => {
  const { r, result } = run({ lock: CLEAN, code: `${CODE}<span>Extra badge</span>` });
  assert.equal(r.status, 1);
  const v = of(result, "unmapped-code-content");
  assert.equal(v.length, 1);
  assert.equal(v[0].item, "Extra badge");
  assert.equal(v[0].side, "code");
});

test("rules 1 and 2: two equal items are two instances; a hidden item is none", () => {
  const { result } = run({ lock: `${CLEAN}<i>Dup</i><i>Dup</i><i style="display:none">Hidden</i>`, code: CODE });
  assert.deepEqual(of(result, "unmapped-lock-content").map((x) => x.item), ["Dup", "Dup"]);
});

test("rule 2: a control's own label is one item, not a control plus a text", () => {
  const { result } = run({ lock: CLEAN, code: `${CODE}<button>Delete</button>` });
  const v = of(result, "unmapped-code-content");
  assert.deepEqual(v.map((x) => `${x.item_kind}:${x.item}`), ["control:Delete"]);
});

test("rule 4 duplicate-block: a non-repeat block that matches two elements, on either side", () => {
  const twice = `<p class="a">Alpha</p><p class="a">Alpha</p><p class="b">Beta</p>`;
  const lockSide = run({ lock: twice, code: CODE });
  assert.deepEqual(of(lockSide.result, "duplicate-block").map((x) => `${x.block}:${x.side}`), ["a:lock"]);
  const codeSide = run({ lock: CLEAN, code: `<p data-testid="a">Alpha</p><p data-testid="a">Alpha</p><p data-testid="b">Beta</p>` });
  assert.deepEqual(of(codeSide.result, "duplicate-block").map((x) => `${x.block}:${x.side}`), ["a:code"]);
});

test("rule 4: a repeat block may match several elements", () => {
  const rows = `<p class="a">Alpha</p><p class="a">Alpha</p><p class="b">Beta</p>`;
  const { result } = run({ lock: rows, code: rows.replaceAll('class="a"', 'data-testid="a"').replaceAll('class="b"', 'data-testid="b"'), blocks: { a: { selector: ".a", repeat: true }, b: { selector: ".b" } } });
  assert.equal(of(result, "duplicate-block").length, 0);
});

test("rule 1: an unmapped lock control that wraps a mapped block is still one control item", () => {
  const { r, result } = run({ lock: `<button><span class="a">Alpha</span></button><p class="b">Beta</p>`, code: CODE });
  assert.equal(r.status, 1);
  assert.deepEqual(of(result, "unmapped-lock-content").map((x) => `${x.item_kind}:${x.item}`), ["control:Alpha"]);
});

test("rule 2: an unmapped code control that wraps a mapped block is still one control item", () => {
  const { r, result } = run({ lock: CLEAN, code: `<button><span data-testid="a">Alpha</span></button><p data-testid="b">Beta</p>` });
  assert.equal(r.status, 1);
  assert.deepEqual(of(result, "unmapped-code-content").map((x) => `${x.item_kind}:${x.item}`), ["control:Alpha"]);
});
