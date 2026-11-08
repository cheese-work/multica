// CHE-2985 (B2 of CHE-2970): fixed screen list, profiles, finding format and weekly cap
// for the UX explorer. The run definition is skills/ux-explorer/SKILL.md; this file is the
// part that can be checked by a test instead of trusted as prose.

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

/** Highest pain first, older first on a tie. The first `cap` publish; the rest wait for next week. */
export function selectWeekly(pending, cap = WEEKLY_CAP) {
  const ranked = [...pending].sort((a, b) => b.pain - a.pain || String(a.found_at).localeCompare(String(b.found_at)));
  return { publish: ranked.slice(0, cap), carryOver: ranked.slice(cap) };
}
