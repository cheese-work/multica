// CHE-2985 (B2 of CHE-2970): fixed screen list, profiles, finding format and weekly cap
// for the UX explorer. The run definition is skills/ux-explorer/SKILL.md; this file is the
// part that can be checked by a test instead of trusted as prose.

import { closeSync, fstatSync, lstatSync, openSync, readSync } from "node:fs";
import { join } from "node:path";

/** The 10 sandbox screens, in visiting order. `{ws}` is the sandbox slug. */
export const SANDBOX_SLUG = "ux-explorer-sandbox";
export const ROUTES = [
  { id: "issues", path: "/{ws}/issues" },
  { id: "issue-detail", path: "/{ws}/issues/<id>" }, // <id>: the sandbox issue with the most comments
  { id: "my-issues", path: "/{ws}/my-issues" },
  { id: "inbox", path: "/{ws}/inbox" },
  { id: "chat", path: "/{ws}/chat" },
  { id: "projects", path: "/{ws}/projects" },
  { id: "project-detail", path: "/{ws}/projects/<id>" }, // <id>: the first project in the list
  { id: "agents", path: "/{ws}/agents" },
  { id: "autopilots", path: "/{ws}/autopilots" },
  { id: "settings", path: "/{ws}/settings" },
];

/** Same two profiles as playwright.perf.config.ts (CHE-2976). */
export const PROFILES = {
  desktop: { viewport: "1440x900", touch: false },
  phone: { device: "Pixel 7", viewport: "412x915", touch: true },
};

export const WEEKLY_CAP = 20;
const FORM_FACTORS = ["desktop", "phone", "both"];
const profilesOf = (ff) => (ff === "both" ? ["desktop", "phone"] : [ff]);
// A bare file name: no directory part, no leading dot, so it cannot leave findings/.
const SHOT_NAME = /^[A-Za-z0-9_][A-Za-z0-9._-]*\.png$/;
const text = (v) => typeof v === "string" && v.trim() !== "";

/** Returns a list of problems; an empty list means the finding may be published. */
export function validateFinding(f) {
  const errs = [];
  if (!FORM_FACTORS.includes(f.form_factor)) errs.push("form_factor must be desktop, phone or both");
  if (!text(f.title)) errs.push("title is required");
  if (f.class === 1) {
    if (!Array.isArray(f.steps) || !f.steps.length || !f.steps.every(text)) errs.push("class 1 needs steps");
    const m = f.measurement;
    if (!m || !text(m.metric) || typeof m.value !== "number" || !text(m.profile)) {
      errs.push("class 1 needs a measurement {metric, value, profile}");
    }
  } else if (f.class === 2) {
    for (const p of profilesOf(f.form_factor)) {
      if (!f.screenshots?.some((s) => s.profile === p && text(s.file))) errs.push(`class 2 needs a before screenshot for ${p}`);
    }
    for (const s of f.screenshots ?? []) {
      if (!SHOT_NAME.test(String(s.file))) errs.push(`screenshot file must be a plain .png name inside findings/: ${JSON.stringify(s.file)}`);
    }
    if (!text(f.problem)) errs.push("class 2 needs a problem");
    const o = f.options;
    if (!Array.isArray(o) || o.length < 2 || o.length > 3) errs.push("class 2 needs 2-3 options");
    else if (!o.every((x) => text(x.title) && text(x.critique))) errs.push("every option needs a title and a critique");
    else if (!Number.isInteger(f.recommendation) || f.recommendation < 1 || f.recommendation > o.length) {
      errs.push("recommendation must be the 1-based number of an option");
    }
    if (!Number.isInteger(f.pain) || f.pain < 1 || f.pain > 10) errs.push("pain must be an integer 1-10");
  } else {
    errs.push("class must be 1 or 2");
  }
  return errs;
}

/** Where E's PAT lives (CHE-2977). Only phase 1 may read it. */
export const PAT_PATH = "/home/congvc/.multica-explorer/explorer.pat";
export const SANDBOX_ORIGIN_ENV = "UXS_SANDBOX_ORIGIN";
const SECRET_ENV = /^(MULTICA_TOKEN|EXPLORER_PAT|UXS_PAT|.*_PAT)$/;

/** Phase 1 browser guard: install on the context (`context.route`) and abort when this is false. Read-only, sandbox origin only. */
export function allowRequest(method, url, origin) {
  try {
    return ["GET", "HEAD"].includes(String(method).toUpperCase()) && new URL(url).origin === new URL(origin).origin;
  } catch {
    return false;
  }
}

/** Phase 2 gate: run in the publication process before it reads findings/. Empty list = E's credential is not reachable. */
export function isolationProblems({ env, canRead }) {
  const errs = [];
  for (const k of Object.keys(env)) if (SECRET_ENV.test(k)) errs.push(`credential variable ${k} is set`);
  if (canRead(PAT_PATH)) errs.push(`${PAT_PATH} is readable by this process`);
  return errs;
}

/** Files named by a class 2 finding must be regular PNG files directly inside `dir` (no symlink, no other content). */
export function checkScreenshotFiles(dir, f) {
  const errs = [];
  for (const s of f.screenshots ?? []) {
    const name = String(s.file);
    if (!SHOT_NAME.test(name)) { errs.push(`bad screenshot name ${JSON.stringify(name)}`); continue; }
    const p = join(dir, name);
    try {
      if (!lstatSync(p).isFile()) { errs.push(`${name} is not a regular file`); continue; }
      const fd = openSync(p, "r");
      try {
        const head = Buffer.alloc(8);
        if (!fstatSync(fd).isFile() || readSync(fd, head, 0, 8, 0) < 8 || !head.equals(PNG_MAGIC)) errs.push(`${name} is not a PNG`);
      } finally {
        closeSync(fd);
      }
    } catch {
      errs.push(`${name} is missing or unreadable`);
    }
  }
  return errs;
}
const PNG_MAGIC = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);

/**
 * Highest pain first, older first on a tie. At most `cap - published` publish this week; the rest wait.
 * `published` = proposals already in this week's issue. An unknown count (NaN) publishes none: fail closed.
 */
export function selectWeekly(pending, cap = WEEKLY_CAP, published = 0) {
  const room = Number.isFinite(published) ? Math.max(0, cap - published) : 0;
  const ranked = [...pending].sort((a, b) => b.pain - a.pain || String(a.found_at).localeCompare(String(b.found_at)));
  return { publish: ranked.slice(0, room), carryOver: ranked.slice(room) };
}
