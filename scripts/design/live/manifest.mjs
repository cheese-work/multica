#!/usr/bin/env node
/**
 * Live route manifest checks (CHE-5793 C9, step 31).
 *
 * Run: node scripts/design/live/manifest.mjs --check
 * Rules: route-unlisted, tab-unlisted, orphan-entry, duplicate-live-key,
 * unknown-profile, unknown-theme, missing-expectation, invalid-entry, invalid-header.
 */
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { parse } from "yaml";

import { buildKey } from "./keys.mjs";

const MANIFEST = "design/live-routes.yml";
const DEVICES = "design/devices.yml";
const APP = "apps/web/app";
const SCOPE = ["[workspaceSlug]", "(auth)"];
const SETTINGS_PAGE = "packages/views/settings/components/settings-page.tsx";
const SETTINGS_ROUTE_PAGE = "apps/web/app/[workspaceSlug]/(dashboard)/settings/page.tsx";
const HEADER = { project: "5e621e12-eeb4-40a5-9571-25657d5b0ec5", canvas: "mRAnnjvGod", page: "Live" };
const AUTH = new Set(["anonymous", "owner", "new-user", "no-workspace", "invitee"]);

// apps/web/app/[workspaceSlug]/(dashboard)/issues/[id]/page.tsx -> /[workspaceSlug]/issues/[id]
export const routeOf = (file) =>
  "/" + file.replace(/^apps\/web\/app\//, "").replace(/\/?page\.tsx$/, "").split("/").filter((s) => s && !/^\(.*\)$/.test(s)).join("/");

export function scanRoutes(root) {
  const out = [];
  const walk = (rel) => {
    for (const e of readdirSync(join(root, rel), { withFileTypes: true })) {
      const next = `${rel}/${e.name}`;
      if (e.isDirectory()) walk(next);
      else if (e.name === "page.tsx") out.push(routeOf(next));
    }
  };
  for (const s of SCOPE) walk(`${APP}/${s}`);
  return out.sort();
}

// Tab ids: entry("<value>" calls in the settings page, plus extraDeviceTabs values of the web route page.
export function scanTabs(root) {
  const read = (p) => readFileSync(join(root, p), "utf8");
  const ids = [
    ...[...read(SETTINGS_PAGE).matchAll(/\bentry\(\s*"([^"]+)"/g)].map((m) => m[1]),
    ...[...read(SETTINGS_ROUTE_PAGE).matchAll(/\bvalue:\s*"([^"]+)"/g)].map((m) => m[1]),
  ];
  return [...new Set(ids)].sort();
}

const isStr = (v) => typeof v === "string" && v.length > 0;
const validMarker = (m) => m && (isStr(m.testid) || (isStr(m.role) && isStr(m.name)));

// scan: { routes, tabs, profiles }. Returns [{ code, detail }].
export function validateManifest(manifest, { routes, tabs, profiles }) {
  const errors = [];
  const err = (code, detail) => errors.push({ code, detail });
  for (const [k, v] of Object.entries(HEADER)) if (manifest?.[k] !== v) err("invalid-header", `${k} must be ${v}`);
  const entries = Array.isArray(manifest?.entries) ? manifest.entries : [];
  const seenKeys = new Set();
  const coveredRoutes = new Set();
  const coveredTabs = new Set();
  for (const [i, e] of entries.entries()) {
    const id = `entry ${i} (${e?.screen ?? "?"}/${e?.state ?? "?"})`;
    if (!validMarker(e?.expect?.marker)) err("missing-expectation", `${id} has no expect.marker (role and name, or testid)`);
    const missing = ["screen", "state", "path"].filter((f) => !isStr(e?.[f]));
    if (!isStr(e?.expect?.url)) missing.push("expect.url");
    if (!e?.seed || typeof e.seed !== "object") missing.push("seed");
    if (!Array.isArray(e?.scenes)) missing.push("scenes");
    if (!AUTH.has(e?.auth)) missing.push("auth");
    if (isStr(e.state) && isStr(e.path)) {
      if (e.state.startsWith("tab-")) {
        const tab = e.state.slice(4);
        if (tabs.includes(tab)) coveredTabs.add(tab);
        else err("orphan-entry", `${id}: no settings tab ${tab}`);
      } else {
        const route = e.path.split("?")[0];
        if (routes.includes(route)) coveredRoutes.add(route);
        else err("orphan-entry", `${id}: no in-scope page.tsx for ${route}`);
      }
    }
    if (missing.length) {
      err("invalid-entry", `${id}: bad or missing ${missing.join(", ")}`);
      continue;
    }
    try {
      const key = buildKey(
        { source: "route", screen: e.screen, state: e.state, profile: e.profile ?? manifest.profile, theme: e.theme ?? manifest.theme },
        profiles,
      );
      if (seenKeys.has(key)) err("duplicate-live-key", key);
      seenKeys.add(key);
    } catch (x) {
      err(x.code ?? "invalid-entry", `${id}: ${x.message}`);
    }
  }
  for (const r of routes) if (!coveredRoutes.has(r)) err("route-unlisted", r);
  for (const t of tabs) if (!coveredTabs.has(t)) err("tab-unlisted", t);
  const seen = new Set();
  return errors.filter((x) => !seen.has(x.code + x.detail) && seen.add(x.code + x.detail));
}

export function check(root) {
  const manifest = parse(readFileSync(join(root, MANIFEST), "utf8"));
  const profiles = parse(readFileSync(join(root, DEVICES), "utf8")).profiles;
  const routes = scanRoutes(root);
  const tabs = scanTabs(root);
  return { errors: validateManifest(manifest, { routes, tabs, profiles }), routes: routes.length, tabs: tabs.length };
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  if (!process.argv.includes("--check")) {
    console.error("usage: node scripts/design/live/manifest.mjs --check");
    process.exit(2);
  }
  const { errors, routes, tabs } = check(fileURLToPath(new URL("../../../", import.meta.url)));
  for (const x of errors) console.error(`${x.code}: ${x.detail}`);
  if (errors.length) process.exit(1);
  console.log(`live-routes: ${routes} routes, ${tabs} tabs`);
}
