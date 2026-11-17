import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { buildIndexHtml, checkScene, enumerateRoutes, findDataUri, frameName, ICT, pageName } from "./snapshot-screens.mjs";

const touch = (root, rel) => {
  const f = join(root, rel);
  mkdirSync(join(f, ".."), { recursive: true });
  writeFileSync(f, "export default function Page(){return null}\n");
};

test("ICT writes the ICT suffix and never a bare UTC or Z time", () => {
  assert.equal(ICT(new Date("2026-11-17T03:30:00Z")), "17 Nov 2026 10:30 ICT");
});

test("page and frame names follow the issue contract", () => {
  assert.equal(pageName(new Date("2026-11-17T03:30:00Z"), "b8c0c83e77"), "Snapshot · 2026-11-17 · b8c0c83e77");
  assert.equal(
    frameName({ screen: "issues", state: "list", sha: "b8c0c83e77" }),
    "Snapshot · issues · list · web-1440 · dark · b8c0c83e77",
  );
});

test("enumerateRoutes lists in-app and auth page files, drops route groups, flags dynamic segments", () => {
  const app = mkdtempSync(join(tmpdir(), "snap-routes-"));
  touch(app, "[workspaceSlug]/(dashboard)/issues/page.tsx");
  touch(app, "[workspaceSlug]/(dashboard)/issues/[id]/page.tsx");
  touch(app, "(auth)/login/page.tsx");
  touch(app, "(landing)/about/page.tsx");
  const byRoute = Object.fromEntries(enumerateRoutes(app).map((r) => [r.route, r]));
  assert.deepEqual(Object.keys(byRoute).sort(), ["/[workspaceSlug]/issues", "/[workspaceSlug]/issues/[id]", "/about", "/login"]);
  assert.equal(byRoute["/[workspaceSlug]/issues"].scope, "in-app");
  assert.equal(byRoute["/[workspaceSlug]/issues"].dynamic, false);
  assert.equal(byRoute["/[workspaceSlug]/issues/[id]"].dynamic, true);
  assert.equal(byRoute["/login"].scope, "auth");
  assert.equal(byRoute["/about"].scope, "other");
});

test("checkScene fails on a wrong final URL or a missing landmark, with the cause", () => {
  const ok = { finalUrl: "http://h/dev/issues", expectedPath: "/dev/issues", landmarkFound: true };
  assert.deepEqual(checkScene(ok), { ok: true, cause: "" });
  const redirected = checkScene({ ...ok, finalUrl: "http://h/login" });
  assert.equal(redirected.ok, false);
  assert.match(redirected.cause, /final URL \/login, expected \/dev\/issues/);
  const missing = checkScene({ ...ok, landmarkFound: false });
  assert.equal(missing.ok, false);
  assert.match(missing.cause, /landmark/);
});

test("findDataUri catches a data: URI in src, href or CSS url()", () => {
  assert.equal(findDataUri('<img src="data:image/png;base64,AAAA">'), true);
  assert.equal(findDataUri("<div style=\"background:url('data:image/png;base64,AA')\">"), true);
  assert.equal(findDataUri('<img src="https://doop/assets/a.png">'), false);
});

test("index frame has the banner, every route with frame and cause, escapes HTML, holds no data: URI", () => {
  const html = buildIndexHtml({
    sha: "b8c0c83e77",
    capturedAt: new Date("2026-11-17T03:30:00Z"),
    rows: [
      { route: "/[workspaceSlug]/issues", status: "captured", frame: "Snapshot · issues · list · web-1440 · dark · b8c0c83e77", cause: "" },
      { route: "/[workspaceSlug]/agents/[id]", status: "not captured", frame: "", cause: "no e2e seed gives an agent <record>" },
    ],
  });
  assert.match(html, /Snapshot of `?b8c0c83e77`?, captured 17 Nov 2026 10:30 ICT\. Not Live\. Archived on 24 Nov 2026 or at the first Live publish\./);
  assert.match(html, /\/\[workspaceSlug\]\/issues/);
  assert.match(html, /no e2e seed gives an agent &lt;record&gt;/);
  assert.equal(findDataUri(html), false);
  assert.ok(html.includes("1 of 2 captured") || html.includes("captured 1 / listed 2"));
});
