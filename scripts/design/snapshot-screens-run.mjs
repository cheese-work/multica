// Runner for snapshot-screens.mjs (CHE-6731). Seeds a LOCAL instance the way e2e/fixtures.ts does,
// then opens each route in a new browser context and captures one 1440x900 dark viewport PNG.
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { chromium } from "@playwright/test";
import pg from "pg";
import { scenes } from "./snapshot-screens-scenes.mjs";
import { PROFILE, buildIndexHtml, checkScene, enumerateRoutes, frameName, ICT } from "./snapshot-screens.mjs";

const ROOT = fileURLToPath(new URL("../../", import.meta.url));

export function loadEnv() {
  const env = {};
  for (const l of readFileSync(join(ROOT, ".env.worktree"), "utf8").split("\n")) {
    const m = /^([A-Z_][A-Z0-9_]*)=(.*)$/.exec(l);
    if (m) env[m[1]] = m[2].replace(/^"|"$/g, "");
  }
  return env;
}

// Mirrors TestApiClient.login (e2e/fixtures.ts): verification code read from the local DB.
async function login(api, db, env, email, name) {
  await db.query("DELETE FROM verification_code WHERE email = $1", [email]);
  const send = await fetch(`${api}/auth/send-code`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ email }) });
  if (!send.ok) throw new Error(`send-code ${send.status}`);
  const row = await db.query("SELECT code FROM verification_code WHERE email = $1 AND used = FALSE AND expires_at > now() ORDER BY created_at DESC LIMIT 1", [email]);
  const code = env.MULTICA_DEV_VERIFICATION_CODE?.trim() || row.rows[0]?.code;
  const res = await fetch(`${api}/auth/verify-code`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ email, code }) });
  if (!res.ok) throw new Error(`verify-code ${res.status}`);
  const { token, user } = await res.json();
  if (user?.name !== name) await fetch(`${api}/api/me`, { method: "PATCH", headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` }, body: JSON.stringify({ name }) });
  return token;
}

export async function seed(api, db, env) {
  const stamp = Date.now().toString(36);
  const SLUG = `snapshot-${stamp}`;
  const ownerEmail = `snapshot-owner-${stamp}@multica.ai`;
  const peerEmail = `snapshot-peer-${stamp}@multica.ai`;
  const freshEmail = `snapshot-fresh-${stamp}@multica.ai`;
  const token = await login(api, db, env, ownerEmail, "Snapshot Owner");
  const peerToken = await login(api, db, env, peerEmail, "Snapshot Peer");
  const freshToken = await login(api, db, env, freshEmail, "Snapshot Fresh");
  const auth = { "Content-Type": "application/json", Authorization: `Bearer ${token}` };
  const ws = await (await fetch(`${api}/api/workspaces`, { method: "POST", headers: auth, body: JSON.stringify({ name: "Snapshot Seed", slug: SLUG }) })).json();
  const wsAuth = { ...auth, "X-Workspace-Slug": SLUG, "X-Workspace-ID": ws.id };
  const onboard = (email) => db.query(`UPDATE "user" SET onboarded_at = COALESCE(onboarded_at, now()), onboarding_questionnaire = COALESCE(onboarding_questionnaire, '{}'::jsonb) || '{"source":["friends_colleagues"],"source_other":null,"source_skipped":false}'::jsonb WHERE email = $1`, [email]);
  await onboard(ownerEmail);
  await onboard(peerEmail); // fresh user stays not onboarded: it owns /onboarding and /workspaces/new
  const post = async (path, body) => {
    const r = await fetch(`${api}${path}`, { method: "POST", headers: wsAuth, body: JSON.stringify(body) });
    if (!r.ok) throw new Error(`POST ${path} ${r.status} ${await r.text()}`);
    return r.json();
  };
  const project = await post("/api/projects", { title: "Snapshot project" });
  const issue = await post("/api/issues", { title: "Snapshot issue", project_id: project.id });
  await post(`/api/issues/${issue.id}/comments`, { content: "Snapshot comment" });
  const runtime = await db.query(`INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, owner_id, last_seen_at) SELECT $1, $2, 'local', 'claude', 'online', id, now() FROM "user" WHERE email = $3 RETURNING id`, [ws.id, "Snapshot runtime", ownerEmail]);
  const member = await db.query(`INSERT INTO member (workspace_id, user_id, role) SELECT $1, id, 'member' FROM "user" WHERE email = $2 RETURNING user_id`, [ws.id, peerEmail]);
  return { slug: SLUG, token, freshToken, peerToken, ids: { issue: issue.identifier ?? issue.id, project: project.id, runtime: runtime.rows[0].id, member: member.rows[0].user_id } };
}

// Tab values from the source, split into the ones the seeded owner sees in the nav and the rest.
async function discoverSettingsTabs(browser, base, s) {
  const src = readFileSync(join(ROOT, "packages/views/settings/components/settings-page.tsx"), "utf8");
  const all = new Set([...src.matchAll(/entry\(\s*"([a-z][a-z-]*)"/g)].map((m) => m[1]));
  all.add("site-search"); // extraDeviceTabs in apps/web/app/[workspaceSlug]/(dashboard)/settings/page.tsx
  const ctx = await browser.newContext({ viewport: { width: PROFILE.width, height: PROFILE.height } });
  await ctx.addInitScript((t) => localStorage.setItem("multica_token", t), s.token);
  await ctx.addCookies([{ name: "multica_logged_in", value: "1", url: base }]);
  const page = await ctx.newPage();
  await page.goto(`${base}/${s.slug}/settings`, { waitUntil: "load", timeout: 180_000 });
  await page.locator('[data-testid^="settings-nav-"]').first().waitFor({ timeout: 60_000 });
  const visible = await page.$$eval('[data-testid^="settings-nav-"]', (els) => [...new Set(els.map((e) => e.getAttribute("data-testid").slice("settings-nav-".length)))]);
  await ctx.close();
  return { visible, hidden: [...all].filter((t) => !visible.includes(t)) };
}

// next dev compiles a route on first visit; visit every capture path once so the real pass is not timing the compiler.
async function warm(browser, base, s, scenes, settingsTabs, routes) {
  const ctx = await browser.newContext({ viewport: { width: PROFILE.width, height: PROFILE.height } });
  await ctx.addInitScript((t) => localStorage.setItem("multica_token", t), s.token);
  await ctx.addCookies([{ name: "multica_logged_in", value: "1", url: base }]);
  for (const r of routes) {
    for (const c of scenes(r, { slug: s.slug, ids: s.ids, settingsTabs }).captures?.slice(0, 1) ?? []) {
      const page = await ctx.newPage();
      await page.goto(`${base}${c.path}`, { waitUntil: "load", timeout: 180_000 }).catch(() => {});
      await page.waitForTimeout(1500);
      await page.close();
    }
  }
  await ctx.close();
}

export async function run(argv) {
  const arg = (k) => argv[argv.indexOf(`--${k}`) + 1];
  const [base, api, out, sha] = ["base", "api", "out", "sha"].map(arg);
  if (!base || !api || !out || !sha) throw new Error("usage: --base <web> --api <api> --out <dir> --sha <short sha>");
  if (!/^https?:\/\/(localhost|127\.0\.0\.1)(:|\/|$)/.test(base) || !/^https?:\/\/(localhost|127\.0\.0\.1)(:|\/|$)/.test(api)) {
    throw new Error("refusing: base and api must be localhost (no production or mirrored data)");
  }
  mkdirSync(out, { recursive: true });
  const env = loadEnv();
  const db = new pg.Client(env.DATABASE_URL);
  await db.connect();
  const startedAt = new Date();
  const s = await seed(api, db, env);
  await db.end();
  writeFileSync(join(out, "seed.json"), JSON.stringify({ slug: s.slug, ids: s.ids }, null, 2));
    const browser = await chromium.launch();
  const rows = [];
  let outOfScope = 0;
  const settingsTabs = await discoverSettingsTabs(browser, base, s);
  await warm(browser, base, s, scenes, settingsTabs, enumerateRoutes(join(ROOT, "apps/web/app")));
  const routes = enumerateRoutes(join(ROOT, "apps/web/app"));
  for (const r of routes) {
    if (r.scope === "other") { outOfScope += 1; continue; } // landing and OAuth bind pages are not in-app or (auth)
    const scene = scenes(r, { slug: s.slug, ids: s.ids, settingsTabs });
    if (!scene.captures) {
      rows.push({ route: r.route, scope: r.scope, status: "not captured", frame: "", cause: scene.cause, file: "" });
      continue;
    }
    for (const x of scene.extra ?? []) rows.push({ route: x.route, scope: r.scope, status: "not captured", frame: "", cause: x.cause, file: "" });
    for (const c of scene.captures) {
      const name = frameName({ screen: c.screen, state: c.state, sha });
      const row = { route: c.label ?? r.route, scope: r.scope, status: "not captured", frame: "", cause: "", file: "" };
      rows.push(row);
      const ctx = await browser.newContext({ viewport: { width: PROFILE.width, height: PROFILE.height }, deviceScaleFactor: PROFILE.dpr, colorScheme: "dark" });
      const token = { owner: s.token, fresh: s.freshToken, none: null }[c.auth];
      await ctx.addInitScript(([t]) => { localStorage.setItem("theme", "dark"); if (t) localStorage.setItem("multica_token", t); }, [token]);
      if (token) await ctx.addCookies([{ name: "multica_logged_in", value: "1", url: base }]);
      const page = await ctx.newPage();
      try {
        await page.goto(`${base}${c.path}`, { waitUntil: "load", timeout: 120_000 });
        if (c.after) await c.after(page);
        const landmarkFound = await page.locator(c.landmark).first().waitFor({ state: "visible", timeout: 20_000 }).then(() => true, () => false);
        await page.waitForLoadState("networkidle", { timeout: 4000 }).catch(() => {});
        await page.waitForTimeout(800);
        const verdict = checkScene({ finalUrl: page.url(), expectedPath: c.expectedPath ?? c.path.split("?")[0], landmarkFound });
        if (!verdict.ok) row.cause = verdict.cause;
        else {
          const file = `${name.replace(/[^\w.-]+/g, "_")}.png`;
          await page.addStyleTag({ content: "nextjs-portal{display:none!important}" }); // next dev indicator is not part of the screen
          await page.screenshot({ path: join(out, file) });
          Object.assign(row, { status: "captured", frame: name, file });
        }
      } catch (e) {
        row.cause = `${e.name}: ${String(e.message).split("\n")[0]}`;
      } finally {
        await ctx.close();
      }
    }
  }
  await browser.close();
  const endedAt = new Date();
  const captured = rows.filter((x) => x.status === "captured").length;
  const manifest = { sha, base, startedAt: startedAt.toISOString(), endedAt: endedAt.toISOString(), startedICT: ICT(startedAt), endedICT: ICT(endedAt), captured, listed: rows.length, outOfScopeRoutes: outOfScope, rows };
  writeFileSync(join(out, "manifest.json"), JSON.stringify(manifest, null, 2));
  writeFileSync(join(out, "index.html"), buildIndexHtml({ sha, capturedAt: endedAt, rows }));
  console.log(`captured ${captured} / listed ${rows.length}; ${manifest.startedICT} to ${manifest.endedICT}`);
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) await run(process.argv.slice(2));
