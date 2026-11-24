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
  return checkUiMap({ head, changes, base: withBase ? gitTree(cwd, baseSha) : null });
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
  const withMap = { ...beta, "tests/fidelity/beta.blocks.yml": "topic: beta\n" };
  assert.deepEqual(run(t, { entries: [entry(), good], baseEntries: [entry()], head: withMap }).errors, []);
  assert.deepEqual(reasons(run(t, { entries: [entry(), good], baseEntries: [entry()], head: beta })), ["new-unmapped"]);
  const shipped = { ...good, state: "shipped" };
  assert.deepEqual(reasons(run(t, { entries: [entry(), shipped], baseEntries: [entry()], head: withMap })), ["new-unmapped"]);
});
test("rule 4: a building entry that exists at seed with the same pin is not new", (t) => {
  const r = run(t, { entries: [entry({ state: "building", paths: ["packages/views/issues/**"], blocks_map: "tests/fidelity/alpha.blocks.yml" })] });
  assert.deepEqual(r.errors, []);
});

// ---- rule 4 across PRs: a pin that moved since seed_sha may later become shipped ----
const GAMMA = {
  "design/alpha/approved/LOCK.json": lock(PIN_C),
  "tests/fidelity/alpha.blocks.yml": "topic: alpha\n",
};
const repinned = (over = {}) => entry({ lock_export_sha256: PIN_C, state: "building", blocks_map: "tests/fidelity/alpha.blocks.yml", ...over });
test("rule 4: a design PR that re-pins a seeded lock passes as building, and fails as unmapped or shipped", (t) => {
  assert.deepEqual(run(t, { entries: [repinned()], baseEntries: [entry()], head: GAMMA }).errors, []);
  assert.deepEqual(reasons(run(t, { entries: [repinned({ state: "unmapped" })], baseEntries: [entry()], head: GAMMA })), ["new-unmapped"]);
  assert.deepEqual(reasons(run(t, { entries: [repinned({ state: "shipped" })], baseEntries: [entry()], head: GAMMA })), ["new-unmapped"]);
});
test("rule 4: a later PR may switch a re-pinned building entry to shipped (pin unchanged since the PR base)", (t) => {
  const r = run(t, { seedFiles: SEED_FILES, base: GAMMA, entries: [repinned({ state: "shipped" })], baseEntries: [repinned()] });
  assert.deepEqual(r.errors, []);
});
test("rule 4: a shipped entry whose lock moves again in a later PR must go back to building", (t) => {
  const delta = { "design/alpha/approved/LOCK.json": lock(PIN_B) };
  const spec = { base: GAMMA, baseEntries: [repinned({ state: "shipped" })], head: delta };
  assert.deepEqual(reasons(run(t, { ...spec, entries: [repinned({ state: "shipped", lock_export_sha256: PIN_B })] })), ["new-unmapped"]);
  assert.deepEqual(run(t, { ...spec, entries: [repinned({ lock_export_sha256: PIN_B })] }).errors, []);
});
test("rule 4: a map-only PR that re-pins in place to match a lock-only change still passes only as building", (t) => {
  const spec = { base: GAMMA, baseEntries: [repinned()], head: { "design/alpha/approved/LOCK.json": lock(PIN_B) } };
  assert.deepEqual(reasons(run(t, { ...spec, entries: [repinned({ state: "shipped", lock_export_sha256: PIN_B })] })), ["new-unmapped"]);
});
test("rule 3 canary: a lock-only PR (no map change) fails lock-pin-mismatch", (t) => {
  const r = run(t, { entries: [entry()], baseEntries: [entry()], head: { "design/alpha/approved/LOCK.json": lock(PIN_C) } });
  assert.ok(reasons(r).includes("lock-pin-mismatch"), JSON.stringify(r.errors));
});
test("rule 3 canary: a map-only PR that re-pins without the lock moving fails lock-pin-mismatch", (t) => {
  const r = run(t, { entries: [entry({ lock_export_sha256: PIN_C })], baseEntries: [entry()] });
  assert.ok(reasons(r).includes("lock-pin-mismatch"), JSON.stringify(r.errors));
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

// ---- upstream-owned screens (no design lock) ----
const upstream = (over = {}) => ({ screen: "up-voice", paths: ["packages/views/voice/**"], state: "upstream", ...over });

test("upstream: an entry with no lock owns a new page, a file in a new directory and a new route", (t) => {
  const r = run(t, {
    entries: [entry(), upstream({ paths: ["packages/views/voice/**", "apps/web/app/voice/page.tsx"], routes: ["voice"] })],
    head: {
      "packages/views/voice/panel.tsx": "export const P = 1;\n",
      "apps/web/app/voice/page.tsx": "export default function P() { return null; }\n",
      [ROUTES]: routes("issues", "voice"),
    },
  });
  assert.deepEqual(r.errors, []);
});

test("upstream: without the entry the same changes fail unowned-ui and unowned-screen", (t) => {
  const r = run(t, {
    entries: [entry()],
    head: {
      "packages/views/voice/panel.tsx": "export const P = 1;\n",
      "apps/web/app/voice/page.tsx": "export default function P() { return null; }\n",
      [ROUTES]: routes("issues", "voice"),
    },
  });
  assert.deepEqual(reasons(r), ["unowned-screen", "unowned-screen", "unowned-ui"]);
});

test("upstream: an entry still takes part in rule 1, and needs paths or routes and no lock", (t) => {
  const r = run(t, {
    entries: [entry(), upstream(), upstream({ screen: "up-two", paths: ["packages/views/voice/*.tsx"] })],
    head: { "packages/views/voice/panel.tsx": "export const P = 1;\n" },
  });
  assert.deepEqual(reasons(r), ["ambiguous-owner"]);
  assert.ok(reasons(run(t, { entries: [entry(), upstream({ paths: [] })] })).includes("bad-map"));
  assert.ok(reasons(run(t, { entries: [entry(), upstream({ lock: "alpha", lock_export_sha256: PIN_A })] })).includes("bad-map"));
});

test("upstream: two upstream entries do not trip rule 3 or 4 (no lock folder to own)", (t) => {
  const r = run(t, { entries: [entry(), upstream(), upstream({ screen: "up-two", paths: ["packages/views/other/**"] })] }, { withBase: false });
  assert.deepEqual(r.errors, []);
});

// ---- the v0.10.0 merge (CHE-9575): the 56 files and routes the merge brought in need a named owner ----
const REPORTED_FILES = [
  "apps/web/app/(landing)/blog/page.tsx",
  "apps/web/app/(landing)/careers/page.tsx",
  "apps/web/app/(landing)/customers/page.tsx",
  "apps/web/app/(landing)/press/page.tsx",
  "apps/web/app/(landing)/pricing/page.tsx",
  "apps/web/app/(landing)/product/[slug]/page.tsx",
  "apps/web/app/(landing)/product/page.tsx",
  "apps/web/app/(landing)/security/page.tsx",
  "apps/web/app/(landing)/solutions/[slug]/page.tsx",
  "apps/web/app/(landing)/solutions/page.tsx",
  "apps/web/app/[workspaceSlug]/(dashboard)/issues/[id]/deliverables/[name]/page.tsx",
  "apps/web/components/i18n/bundles/en.tsx",
  "apps/web/components/i18n/bundles/es.tsx",
  "apps/web/components/i18n/bundles/fr.tsx",
  "apps/web/components/i18n/bundles/ja.tsx",
  "apps/web/components/i18n/bundles/ko.tsx",
  "apps/web/components/i18n/bundles/ru.tsx",
  "apps/web/components/i18n/bundles/zh-Hans.tsx",
  "apps/web/components/i18n/bundles/zh-Hant.tsx",
  "apps/web/components/i18n/locale-bundle.tsx",
  "apps/web/features/landing/site/auto-advance.tsx",
  "apps/web/features/landing/site/chat-page.tsx",
  "apps/web/features/landing/site/cli-page.tsx",
  "apps/web/features/landing/site/cloud-page.tsx",
  "apps/web/features/landing/site/cta-band.tsx",
  "apps/web/features/landing/site/demo/chat-visuals.tsx",
  "apps/web/features/landing/site/demo/cli-visuals.tsx",
  "apps/web/features/landing/site/demo/cloud-visuals.tsx",
  "apps/web/features/landing/site/demo/desktop-visuals.tsx",
  "apps/web/features/landing/site/demo/feature-visuals.tsx",
  "apps/web/features/landing/site/demo/hero-blocks.tsx",
  "apps/web/features/landing/site/demo/hero-board.tsx",
  "apps/web/features/landing/site/demo/hero-demo.tsx",
  "apps/web/features/landing/site/demo/mobile-visuals.tsx",
  "apps/web/features/landing/site/demo/mock-kit.tsx",
  "apps/web/features/landing/site/desktop-page.tsx",
  "apps/web/features/landing/site/footer-wordmark.tsx",
  "apps/web/features/landing/site/home-page.tsx",
  "apps/web/features/landing/site/locale-suggestion.tsx",
  "apps/web/features/landing/site/mobile-page.tsx",
  "apps/web/features/landing/site/pricing-page.tsx",
  "apps/web/features/landing/site/primitives.tsx",
  "apps/web/features/landing/site/site-footer.tsx",
  "apps/web/features/landing/site/site-header.tsx",
  "apps/web/features/landing/site/site-page.tsx",
  "apps/web/features/landing/site/surface-kit.tsx",
  "packages/views/dev-toolbar/actions-panel.tsx",
  "packages/views/dev-toolbar/dev-toolbar.tsx",
  "packages/views/dev-toolbar/panels.tsx",
  "packages/views/dev-toolbar/realtime-panels.tsx",
  "packages/views/star-prompt/star-prompt.tsx",
  "packages/views/voice/dialog-footer.tsx",
  "packages/views/voice/voice-models.tsx",
  "packages/views/voice/voice-ui.tsx",
  "packages/views/voice/workspace-words.tsx",
];
const REPORTED_ROUTES = ["issues/:id/deliverables/:name"];

test("the committed map owns every UI file and desktop route the v0.10.0 merge brought in", () => {
  const changes = {
    files: REPORTED_FILES.map((path) => ({ status: "A", oldPath: path, path })),
    routes: REPORTED_ROUTES,
  };
  const r = checkUiMap({ head: fsTree(process.cwd()), changes });
  assert.deepEqual(r.errors, []);
});
