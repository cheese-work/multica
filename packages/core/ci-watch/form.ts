import type { CIWatchConfig } from "./schema";

/** Panel state as the inputs hold it: text areas, strings for numbers, `kind:id` for the owner. */
export interface CIWatchForm {
  mode: string;
  branches: string;
  events: string[];
  workflowsInclude: string;
  workflowsExclude: string;
  p1: string;
  p2: string;
  p3: string;
  p4: string;
  fixOwner: string;
  notifyMember: string;
  maxReruns: string;
  maxIssues: string;
  maxWakes: string;
}

const MAX_LIST_ITEMS = 50;
const MAX_ITEM_LEN = 200;
const MAX_CAP = 100000;

export function parseList(text: string): string[] {
  return text.split(/[\n,]/).map((s) => s.trim()).filter(Boolean);
}

export function configToForm(c: CIWatchConfig): CIWatchForm {
  return {
    mode: c.mode,
    branches: c.branches.join("\n"),
    events: c.events,
    workflowsInclude: c.workflows_include.join("\n"),
    workflowsExclude: c.workflows_exclude.join("\n"),
    p1: c.p1_mode,
    p2: c.p2_mode,
    p3: c.p3_mode,
    p4: c.p4_mode,
    fixOwner: c.fix_owner_kind && c.fix_owner_id ? `${c.fix_owner_kind}:${c.fix_owner_id}` : "",
    notifyMember: c.notify_member_id ?? "",
    maxReruns: String(c.max_reruns_per_original_run),
    maxIssues: String(c.max_new_issues_per_day),
    maxWakes: String(c.max_owner_wakes_per_hour),
  };
}

const same = (a: string[], b: string[]) => a.length === b.length && a.every((v, i) => v === b[i]);

/** The partial update the server accepts: only fields that differ from `saved`. */
export function formToPatch(saved: CIWatchConfig, f: CIWatchForm): Record<string, unknown> {
  const base = configToForm(saved);
  const patch: Record<string, unknown> = {};
  if (f.mode !== base.mode) patch.mode = f.mode;
  const lists = [
    ["branches", f.branches, saved.branches],
    ["workflows_include", f.workflowsInclude, saved.workflows_include],
    ["workflows_exclude", f.workflowsExclude, saved.workflows_exclude],
  ] as const;
  for (const [key, text, was] of lists) {
    const next = parseList(text);
    if (!same(next, was)) patch[key] = next;
  }
  if (!same(f.events, saved.events)) patch.events = f.events;
  (["p1", "p2", "p3", "p4"] as const).forEach((k) => {
    if (f[k] !== base[k]) patch[`${k}_mode`] = f[k];
  });
  if (f.fixOwner !== base.fixOwner) {
    const [kind, ...id] = f.fixOwner.split(":");
    patch.fix_owner_kind = f.fixOwner ? kind : null;
    patch.fix_owner_id = f.fixOwner ? id.join(":") : null;
  }
  if (f.notifyMember !== base.notifyMember) patch.notify_member_id = f.notifyMember || null;
  const caps = [
    ["max_reruns_per_original_run", f.maxReruns, base.maxReruns],
    ["max_new_issues_per_day", f.maxIssues, base.maxIssues],
    ["max_owner_wakes_per_hour", f.maxWakes, base.maxWakes],
  ] as const;
  for (const [key, next, was] of caps) {
    if (next !== was) patch[key] = Number(next);
  }
  return patch;
}

const badList = (text: string) => {
  const items = parseList(text);
  return items.length > MAX_LIST_ITEMS || items.some((s) => s.length > MAX_ITEM_LEN);
};
const badCap = (s: string) => {
  const n = Number(s);
  return s.trim() === "" || !Number.isInteger(n) || n < 0 || n > MAX_CAP;
};

/** Form fields the server would refuse; the panel disables Save while any remain. */
export function formErrors(f: CIWatchForm): string[] {
  const errors: string[] = [];
  if (badList(f.branches)) errors.push("branches");
  if (badList(f.workflowsInclude)) errors.push("workflowsInclude");
  if (badList(f.workflowsExclude)) errors.push("workflowsExclude");
  if (badCap(f.maxReruns)) errors.push("maxReruns");
  if (badCap(f.maxIssues)) errors.push("maxIssues");
  if (badCap(f.maxWakes)) errors.push("maxWakes");
  return errors;
}
