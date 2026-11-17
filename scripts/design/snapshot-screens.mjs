// One-time dated snapshot of every Multica web screen (CHE-6731, exception to CHE-5793 C4).
// Run on X99 against a local, e2e-seeded instance, never production data. Expires 24 Nov 2026.
//   node scripts/design/snapshot-screens-run.mjs --base <web url> --api <api url> --out <dir> --sha <short sha>
// Writes <out>/<frame>.png plus <out>/manifest.json (every route, captured or not, with its cause).
// Upload and page writing go through the doop MCP, not this script.
import { readdirSync } from "node:fs";
import { join, relative, sep } from "node:path";

export const PROFILE = { id: "web-1440", width: 1440, height: 900, dpr: 1, theme: "dark" };
const ICT_MS = 7 * 3600_000;
const MON = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
const p2 = (n) => String(n).padStart(2, "0");

export function ICT(date) {
  const d = new Date(date.getTime() + ICT_MS);
  return `${d.getUTCDate()} ${MON[d.getUTCMonth()]} ${d.getUTCFullYear()} ${p2(d.getUTCHours())}:${p2(d.getUTCMinutes())} ICT`;
}

export function pageName(date, sha) {
  const d = new Date(date.getTime() + ICT_MS);
  return `Snapshot · ${d.getUTCFullYear()}-${p2(d.getUTCMonth() + 1)}-${p2(d.getUTCDate())} · ${sha}`;
}

export const frameName = ({ screen, state, sha }) => `Snapshot · ${screen} · ${state} · ${PROFILE.id} · ${PROFILE.theme} · ${sha}`;

export const findDataUri = (html) => /data:[a-z]+\/[\w.+-]+[;,]/i.test(html);

// Every page.tsx under apps/web/app. Route groups "(x)" are dropped from the route; scope is
// in-app under [workspaceSlug], auth under (auth), else other (landing, OAuth bind pages).
export function enumerateRoutes(appDir) {
  const files = [];
  const walk = (dir) => {
    for (const e of readdirSync(dir, { withFileTypes: true })) {
      const p = join(dir, e.name);
      if (e.isDirectory()) walk(p);
      else if (e.name === "page.tsx") files.push(relative(appDir, p).split(sep));
    }
  };
  walk(appDir);
  return files
    .map((parts) => {
      const segs = parts.slice(0, -1);
      const route = "/" + segs.filter((s) => !/^\(.*\)$/.test(s)).join("/");
      return {
        file: parts.join("/"),
        route: route === "/" ? "/" : route,
        scope: segs[0] === "[workspaceSlug]" ? "in-app" : segs[0] === "(auth)" ? "auth" : "other",
        dynamic: segs.some((s) => s !== "[workspaceSlug]" && /^\[.+\]$/.test(s)),
      };
    })
    .sort((a, b) => a.route.localeCompare(b.route));
}

export function checkScene({ finalUrl, expectedPath, landmarkFound }) {
  const got = new URL(finalUrl).pathname;
  if (got !== expectedPath) return { ok: false, cause: `final URL ${got}, expected ${expectedPath}` };
  if (!landmarkFound) return { ok: false, cause: `landmark not found on ${expectedPath}` };
  return { ok: true, cause: "" };
}

export function buildIndexHtml({ sha, capturedAt, rows }) {
  const captured = rows.filter((r) => r.status === "captured").length;
  const tr = rows
    .map((r) => `<tr><td><code>${esc(r.route)}</code></td><td>${esc(r.status)}</td><td>${esc(r.frame)}</td><td>${esc(r.cause)}</td></tr>`)
    .join("\n");
  const html = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Snapshot index ${esc(sha)}</title>
<style>body{margin:0;padding:24px;font:14px/1.5 system-ui,sans-serif;background:#111;color:#eee}
.banner{padding:12px 16px;border:1px solid #f5a524;border-radius:8px;margin-bottom:16px}
table{border-collapse:collapse;width:100%}td,th{border:1px solid #333;padding:4px 8px;text-align:left;vertical-align:top}code{font:12px ui-monospace,monospace}</style></head>
<body><div class="banner">Snapshot of ${esc(sha)}, captured ${ICT(capturedAt)}. Not Live. Archived on 24 Nov 2026 or at the first Live publish.</div>
<p>captured ${captured} / listed ${rows.length}</p>
<table><thead><tr><th>Route</th><th>Status</th><th>Frame</th><th>Cause</th></tr></thead><tbody>
${tr}
</tbody></table></body></html>
`;
  if (findDataUri(html)) throw new Error("index frame must not hold a data: URI");
  return html;
}
