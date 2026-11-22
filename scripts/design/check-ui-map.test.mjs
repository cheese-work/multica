import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import test from "node:test";

import { checkUiMap, fsTree, globToRegExp, gitTree, lockPin, readChanges } from "./check-ui-map.mjs";
import { isUiPath } from "./check-ui-pattern-ref.mjs";

// Each test builds a throwaway git repo: `seed` is the commit the map's legacy entry records, `base` the PR base, `head` the PR head.
const HEX = (s) => createHash("sha256").update(s).digest("hex");
const lock = (pin) => JSON.stringify({ export_sha256: pin });
const PIN_A = HEX("a");
const PIN_B = HEX("b");
const PIN_C = HEX("c");
const ROUTES = "apps/desktop/src/renderer/src/routes.tsx";
const routes = (...paths) => `export const routes = [\n${paths.map((p) => `  { path: "${p}", element: null },\n`).join("")}];\n`;

const entry = (over = {}) => ({
  screen: "alpha",
  paths: [],
  lock: "alpha",
  lock_export_sha256: PIN_A,
  state: "unmapped",
  ...over,
});
const mapText = (entries, seed) => {
  const y = (v) => JSON.stringify(v);
  const lines = [`legacy:\n  seed_sha: "${seed}"\nentries:`];
  for (const e of entries) {
    lines.push(`  - ${Object.entries(e).map(([k, v], i) => `${i ? "    " : ""}${k}: ${y(v)}`).join("\n")}`);
  }
  return `${lines.join("\n")}\n`;
};

const SEED_FILES = {
  "design/alpha/approved/LOCK.json": lock(PIN_A),
  "design/alpha/approved/a.html": "<p>a</p>",
  "packages/views/issues/components/old.tsx": "export const Old = 1;\n",
  [ROUTES]: routes("issues"),
};

function fixture(t, { seedFiles = SEED_FILES, base = {}, head = {}, entries, headEntries = entries, baseEntries = headEntries }) {
  const cwd = mkdtempSync(join(tmpdir(), "ui-map-"));
  t.after(() => rmSync(cwd, { recursive: true, force: true }));
  const git = (...args) => execFileSync("git", args, { cwd, encoding: "utf8" });
  const put = (files) => {
    for (const [path, text] of Object.entries(files)) {
      mkdirSync(dirname(join(cwd, path)), { recursive: true });
      writeFileSync(join(cwd, path), text);
    }
  };
  const commit = (m) => {
    git("add", "-A");
    git("commit", "-q", "--allow-empty", "-m", m);
    return git("rev-parse", "HEAD").trim();
  };
  git("init", "-q", "-b", "main");
  git("config", "user.email", "t@example.com");
  git("config", "user.name", "t");
  put(seedFiles);
  const seed = commit("seed");
  put({ ...base, "design/ui-map.yml": mapText(baseEntries ?? [], seed) });
  const baseSha = commit("base");
  put({ ...head, "design/ui-map.yml": mapText(headEntries ?? [], seed) });
  commit("head");
  return { cwd, baseSha, seed };
}

const run = (t, spec, { withBase = true } = {}) => {
  const { cwd, baseSha } = fixture(t, spec);
  const head = gitTree(cwd, "HEAD");
  const changes = withBase ? readChanges(cwd, baseSha, "HEAD") : null;
  return checkUiMap({ head, changes });
};
const reasons = (r) => r.errors.map((e) => e.reason).sort();

test("isUiPath is imported, not copied: this check lists no UI path of its own", () => {
  assert.equal(isUiPath("packages/views/x/y.tsx"), true);
  assert.equal(isUiPath("packages/views/x/y.test.tsx"), false);
});

test("globToRegExp: * stays in a directory, ** crosses them, brackets and parentheses are literal", () => {
  assert.ok(globToRegExp("packages/views/chat/components/decision-*.tsx").test("packages/views/chat/components/decision-summary.tsx"));
  assert.ok(!globToRegExp("packages/views/chat/*.tsx").test("packages/views/chat/components/x.tsx"));
  assert.ok(globToRegExp("packages/views/chat/**").test("packages/views/chat/components/x.tsx"));
  assert.ok(globToRegExp("apps/web/app/**/page.tsx").test("apps/web/app/page.tsx"));
  assert.ok(globToRegExp("apps/web/app/[slug]/(d)/page.tsx").test("apps/web/app/[slug]/(d)/page.tsx"));
  assert.ok(!globToRegExp("apps/web/app/[slug]/page.tsx").test("apps/web/app/s/page.tsx"));
});

test("lockPin: a string pin, or the sha256sum line method for a per-file object", () => {
  assert.equal(lockPin({ export_sha256: PIN_A }), PIN_A);
  const files = { "b.html": PIN_B, "a.html": PIN_A };
  assert.equal(lockPin({ export_sha256: files }), HEX(`${PIN_A}  a.html\n${PIN_B}  b.html\n`));
  assert.equal(lockPin({}), null);
});

// ---- rule 1: exactly one named owner ----
test("rule 1: a UI file under two named entries fails ambiguous-owner", (t) => {
  const r = run(t, {
    entries: [entry({ paths: ["packages/views/issues/**"] }), entry({ screen: "beta", lock: "beta", paths: ["packages/views/issues/components/*.tsx"], state: "building" })],
    seedFiles: { ...SEED_FILES, "design/beta/approved/LOCK.json": lock(PIN_B) },
    head: { "packages/views/issues/components/old.tsx": "export const Old = 2;\n" },
  });
  assert.ok(reasons(r).includes("ambiguous-owner"), JSON.stringify(r.errors));
  assert.match(r.errors.find((e) => e.reason === "ambiguous-owner").message, /alpha.*beta|beta.*alpha/);
});
test("rule 1: a named entry wins over legacy, and a test file needs no owner", (t) => {
  const r = run(t, {
    entries: [entry({ paths: ["packages/views/newdir/**"] })],
    head: { "packages/views/newdir/x.tsx": "export const X = 1;\n", "packages/views/brand-new/x.test.tsx": "test\n" },
  });
  assert.deepEqual(r.errors, []);
});

// ---- rule 2: legacy files, new directories, new screens ----
test("rule 2: a UI file in a directory that does not exist at seed_sha fails unowned-ui", (t) => {
  const r = run(t, { entries: [entry()], head: { "packages/views/newdir/x.tsx": "export const X = 1;\n" } });
  assert.deepEqual(reasons(r), ["unowned-ui"]);
  assert.match(r.errors[0].message, /packages\/views\/newdir\/x\.tsx/);
});
test("rule 2: a legacy-owned file in a directory that exists at seed_sha passes", (t) => {
  const r = run(t, { entries: [entry()], head: { "packages/views/issues/components/new.tsx": "export const N = 1;\n" } });
  assert.deepEqual(r.errors, []);
});
test("rule 2: a new apps/web page.tsx with no named entry fails unowned-screen", (t) => {
  const r = run(t, { entries: [entry()], head: { "apps/web/app/[workspaceSlug]/(dashboard)/fresh/page.tsx": "export default () => null;\n" } });
  assert.deepEqual(reasons(r), ["unowned-screen"]);
});
test("rule 2: a new page.tsx in a directory that exists at seed_sha still needs a named entry", (t) => {
  const r = run(t, {
    seedFiles: { ...SEED_FILES, "apps/web/app/dash/old/page.tsx": "export default () => null;\n" },
    entries: [entry()],
    head: { "apps/web/app/dash/old/inner/page.tsx": "x\n", "apps/web/app/dash/old/other.tsx": "x\n" },
  });
  assert.deepEqual(reasons(r), ["unowned-screen"]);
});
test("rule 2: a new page.tsx owned by a named entry passes", (t) => {
  const r = run(t, {
    entries: [entry({ paths: ["apps/web/app/**/fresh/page.tsx"] })],
    head: { "apps/web/app/dash/fresh/page.tsx": "export default () => null;\n" },
  });
  assert.deepEqual(r.errors, []);
});
test("rule 2: a new path in the desktop routes.tsx fails unowned-screen until an entry lists the route", (t) => {
  const spec = { entries: [entry()], head: { [ROUTES]: routes("issues", "fresh") } };
  assert.deepEqual(reasons(run(t, spec)), ["unowned-screen"]);
  const owned = { entries: [entry({ routes: ["fresh"] })], head: { [ROUTES]: routes("issues", "fresh") } };
  assert.deepEqual(run(t, owned).errors, []);
});
test("rule 2: a moved route path is not a new screen", (t) => {
  const r = run(t, { entries: [entry()], head: { [ROUTES]: routes("issues") + "// edited\n" } });
  assert.deepEqual(r.errors, []);
});

// ---- rule 3: every lock folder has exactly one entry; the pin matches ----
test("rule 3: a lock folder with no entry fails lock-without-owner", (t) => {
  const r = run(t, {
    entries: [entry()],
    head: { "design/zeta/approved/LOCK.json": lock(PIN_B), "design/zeta/approved/z.html": "<p>z</p>" },
  });
  assert.deepEqual(reasons(r), ["lock-without-owner"]);
  assert.match(r.errors[0].message, /zeta/);
});
test("rule 3: two entries for one lock folder fail lock-without-owner", (t) => {
  const r = run(t, { entries: [entry(), entry({ screen: "alpha-two" })] });
  assert.deepEqual(reasons(r), ["lock-without-owner"]);
});
test("rule 3: an entry pin that differs from LOCK.json export_sha256 fails lock-pin-mismatch", (t) => {
  const r = run(t, { entries: [entry({ lock_export_sha256: PIN_B })] });
  assert.ok(reasons(r).includes("lock-pin-mismatch"), JSON.stringify(r.errors));
});
test("rule 3: the pin of a per-file export_sha256 object uses the sha256sum line method", (t) => {
  const files = { "b.html": PIN_B, "a.html": PIN_A };
  const pin = HEX(`${PIN_A}  a.html\n${PIN_B}  b.html\n`);
  const seedFiles = { ...SEED_FILES, "design/multi/approved/LOCK.json": JSON.stringify({ export_sha256: files }) };
  const ok = run(t, { seedFiles, entries: [entry(), entry({ screen: "multi", lock: "multi", lock_export_sha256: pin })] });
  assert.deepEqual(ok.errors, []);
  const bad = run(t, { seedFiles, entries: [entry(), entry({ screen: "multi", lock: "multi", lock_export_sha256: PIN_C })] });
  assert.ok(reasons(bad).includes("lock-pin-mismatch"));
});
test("rule 3: runs without a base too (map-wide check)", (t) => {
  const r = run(t, { entries: [entry({ lock_export_sha256: PIN_B })] }, { withBase: false });
  assert.deepEqual(reasons(r), ["lock-pin-mismatch"].concat(reasons(r).filter((x) => x !== "lock-pin-mismatch")));
  assert.ok(reasons(r).includes("lock-pin-mismatch"));
});

// ---- rule 4: unmapped only when it is the same lock as at seed_sha ----
test("rule 4: unmapped passes for a lock that exists at seed_sha with the same pin", (t) => {
  assert.deepEqual(run(t, { entries: [entry()] }).errors, []);
});
test("rule 4: a new lock folder mapped as unmapped fails new-unmapped", (t) => {
  const r = run(t, {
    entries: [entry(), entry({ screen: "beta", lock: "beta", lock_export_sha256: PIN_B })],
    head: { "design/beta/approved/LOCK.json": lock(PIN_B), "design/beta/approved/b.html": "<p>b</p>" },
  });
  assert.deepEqual(reasons(r), ["new-unmapped"]);
});
test("rule 4: a changed lock pin kept unmapped fails new-unmapped", (t) => {
  const r = run(t, {
    entries: [entry({ lock_export_sha256: PIN_C })],
    head: { "design/alpha/approved/LOCK.json": lock(PIN_C) },
  });
  assert.deepEqual(reasons(r), ["new-unmapped"]);
});
test("rule 4: a new entry as building with an existing blocks map passes; without the map it fails", (t) => {
  const beta = { "design/beta/approved/LOCK.json": lock(PIN_B), "design/beta/approved/b.html": "<p>b</p>" };
  const good = entry({ screen: "beta", lock: "beta", lock_export_sha256: PIN_B, state: "building", blocks_map: "tests/fidelity/beta.blocks.yml" });
  assert.deepEqual(run(t, { entries: [entry(), good], head: { ...beta, "tests/fidelity/beta.blocks.yml": "topic: beta\n" } }).errors, []);
  assert.deepEqual(reasons(run(t, { entries: [entry(), good], head: beta })), ["new-unmapped"]);
  const shipped = { ...good, state: "shipped" };
  assert.deepEqual(reasons(run(t, { entries: [entry(), shipped], head: { ...beta, "tests/fidelity/beta.blocks.yml": "topic: beta\n" } })), ["new-unmapped"]);
});
test("rule 4: a building entry that exists at seed with the same pin is not new", (t) => {
  const r = run(t, { entries: [entry({ state: "building", paths: ["packages/views/issues/**"], blocks_map: "tests/fidelity/alpha.blocks.yml" })] });
  assert.deepEqual(r.errors, []);
});

// ---- a malformed map is one named failure, never a crash ----
test("a map that fails its schema reports bad-map", (t) => {
  assert.ok(reasons(run(t, { entries: [entry({ state: "wip" })] })).includes("bad-map"));
  assert.ok(reasons(run(t, { entries: [entry({ lock_export_sha256: "xyz" })] })).includes("bad-map"));
  assert.ok(reasons(run(t, { entries: [entry({ paths: "packages/**" })] })).includes("bad-map"));
  assert.ok(reasons(run(t, { entries: [entry(), entry()] })).includes("bad-map"));
});
test("a missing map or an unreachable seed_sha reports a named failure", (t) => {
  const { cwd } = fixture(t, { entries: [entry()] });
  const empty = checkUiMap({ head: { ...gitTree(cwd, "HEAD"), read: () => null }, changes: null });
  assert.deepEqual(reasons(empty), ["bad-map"]);
  const tree = gitTree(cwd, "HEAD");
  const map = tree.read("design/ui-map.yml").replace(/seed_sha: "?\w+"?/, `seed_sha: "${"0".repeat(40)}"`);
  const lost = checkUiMap({ head: { ...tree, read: (p) => (p === "design/ui-map.yml" ? map : tree.read(p)) }, changes: null });
  assert.deepEqual(reasons(lost), ["seed-unreachable"]);
});

// ---- the committed map ----
test("the committed design/ui-map.yml passes rules 3 and 4 on the working tree", () => {
  const r = checkUiMap({ head: fsTree(process.cwd()), changes: null });
  assert.deepEqual(r.errors, []);
});
