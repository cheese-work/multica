import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { routeOf, validateManifest } from "./manifest.mjs";

const profiles = [{ id: "web-1440", themes: ["light", "dark"] }];
const header = {
  project: "5e621e12-eeb4-40a5-9571-25657d5b0ec5",
  canvas: "mRAnnjvGod",
  page: "Live",
  profile: "web-1440",
  theme: "dark",
};
const marker = { role: "heading", name: "Issues" };
const entry = (over = {}) => ({
  screen: "ws.issues",
  state: "default",
  path: "/[workspaceSlug]/issues",
  auth: "owner",
  seed: {},
  expect: { url: "^/[^/]+/issues$", marker },
  scenes: [],
  ...over,
});
const tabEntry = (tab, over = {}) =>
  entry({ screen: "ws.settings", state: `tab-${tab}`, path: `/[workspaceSlug]/settings?tab=${tab}`, ...over });
const base = () => ({
  manifest: { ...header, entries: [entry(), entry({ screen: "ws.settings", path: "/[workspaceSlug]/settings" }), tabEntry("profile")] },
  scan: { routes: ["/[workspaceSlug]/issues", "/[workspaceSlug]/settings"], tabs: ["profile"] },
});
const codes = ({ manifest, scan }) => validateManifest(manifest, { ...scan, profiles }).map((e) => e.code);

test("routeOf maps page.tsx to its URL template", () => {
  assert.equal(routeOf("apps/web/app/(auth)/login/page.tsx"), "/login");
  assert.equal(routeOf("apps/web/app/[workspaceSlug]/(dashboard)/issues/[id]/page.tsx"), "/[workspaceSlug]/issues/[id]");
});
test("a complete manifest has no errors", () => {
  assert.deepEqual(codes(base()), []);
});
test("an in-scope page.tsx without an entry is route-unlisted", () => {
  const b = base();
  b.scan.routes.push("/[workspaceSlug]/agents");
  assert.deepEqual(codes(b), ["route-unlisted"]);
});
test("a tab without an entry is tab-unlisted", () => {
  const b = base();
  b.scan.tabs.push("members");
  assert.deepEqual(codes(b), ["tab-unlisted"]);
});
test("two entries with one key are duplicate-live-key", () => {
  const b = base();
  b.manifest.entries.push(entry());
  assert.deepEqual(codes(b), ["duplicate-live-key"]);
});
test("an unknown profile is unknown-profile", () => {
  const b = base();
  b.manifest.profile = "tablet-9";
  assert.ok(codes(b).includes("unknown-profile"));
});
test("an unknown theme is unknown-theme", () => {
  const b = base();
  b.manifest.theme = "sepia";
  assert.ok(codes(b).includes("unknown-theme"));
});
test("an entry without a marker is missing-expectation", () => {
  const b = base();
  delete b.manifest.entries[0].expect.marker;
  assert.deepEqual(codes(b), ["missing-expectation"]);
  b.manifest.entries[0].expect.marker = { role: "heading" };
  assert.deepEqual(codes(b), ["missing-expectation"]);
});
test("a data-testid marker is accepted", () => {
  const b = base();
  b.manifest.entries[0].expect.marker = { testid: "issues-page" };
  assert.deepEqual(codes(b), []);
});
test("an entry that matches no page or tab is orphan-entry", () => {
  const b = base();
  b.manifest.entries.push(entry({ screen: "ws.gone", path: "/[workspaceSlug]/gone" }), tabEntry("gone"));
  assert.deepEqual(codes(b), ["orphan-entry", "orphan-entry"]);
});
test("header must name the C9 project, canvas and page", () => {
  const b = base();
  b.manifest.canvas = "other";
  assert.deepEqual(codes(b), ["invalid-header"]);
});
test("an unknown auth state or a missing field is invalid-entry", () => {
  const b = base();
  b.manifest.entries[0].auth = "root";
  delete b.manifest.entries[1].screen;
  assert.deepEqual(codes(b), ["invalid-entry", "invalid-entry"]);
});

test("on the repo, --check exits 0 and prints route count and tab count", () => {
  const root = fileURLToPath(new URL("../../../", import.meta.url));
  const r = spawnSync("node", ["scripts/design/live/manifest.mjs", "--check"], { cwd: root, encoding: "utf8" });
  assert.equal(r.status, 0, r.stderr + r.stdout);
  assert.match(r.stdout, /^live-routes: \d+ routes, \d+ tabs$/m);
});
