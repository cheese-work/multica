import type { GuardPluginStats, GuardStats } from "@multica/core/types";

export type GuardAction = "fix" | "deny" | "shadow" | "pass" | "unknown";

export interface GuardPolicyPlugin {
  name: string;
  tool: string;
  subcommand: string[];
  action: GuardAction;
  message: string;
  failClosed: boolean;
  builtin: string;
}

export interface GuardReviewRule {
  repo: string;
  github: string;
  reviewers: string[];
}

/** The part of `settings.guard_policy` this page shows. */
export interface GuardPolicyView {
  version: number;
  locked: boolean;
  plugins: GuardPolicyPlugin[];
  review: GuardReviewRule[];
}

export interface GuardRow extends GuardPolicyPlugin {
  /** False for a plugin that has hits but no entry in the stored policy (a built-in default). */
  inPolicy: boolean;
  fixed: number;
  refused: number;
  errors: number;
  shadowHits: number;
  daily: GuardPluginStats["daily"];
}

const record = (value: unknown): Record<string, unknown> | null =>
  value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : null;
const text = (value: unknown): string => (typeof value === "string" ? value : "");
const textList = (value: unknown): string[] =>
  Array.isArray(value) ? value.filter((v): v is string => typeof v === "string") : [];
const ACTIONS = ["fix", "deny", "shadow", "pass"] as const;

/** Null when the workspace stores no policy (the built-in rules run) or the value is not an object. */
export function parseGuardPolicy(settings: unknown): GuardPolicyView | null {
  const policy = record(record(settings)?.guard_policy);
  if (!policy) return null;
  const plugins = (Array.isArray(policy.plugins) ? policy.plugins : []).flatMap((raw): GuardPolicyPlugin[] => {
    const plugin = record(raw);
    const name = text(plugin?.name);
    if (!plugin || !name) return [];
    const match = record(plugin.match);
    const action = ACTIONS.find((a) => a === plugin.action) ?? "unknown";
    return [{
      name,
      tool: text(match?.tool),
      subcommand: textList(match?.subcommand),
      action,
      message: text(plugin.message),
      failClosed: plugin.fail_closed === true,
      builtin: text(plugin.builtin),
    }];
  });
  const review = (Array.isArray(policy.review) ? policy.review : []).flatMap((raw): GuardReviewRule[] => {
    const rule = record(raw);
    const repo = text(rule?.repo);
    return rule && repo ? [{ repo, github: text(rule.github), reviewers: textList(rule.reviewers) }] : [];
  });
  return {
    version: typeof policy.version === "number" ? policy.version : 0,
    locked: policy.locked === true,
    plugins,
    review,
  };
}

/** Stored plugins first, in policy order, then plugins that only have hits. */
export function buildGuardRows(policy: GuardPolicyView | null, stats: GuardStats): GuardRow[] {
  const byName = new Map(stats.plugins.map((p) => [p.plugin, p]));
  const counts = (name: string) => {
    const s = byName.get(name);
    return { fixed: s?.fixed ?? 0, refused: s?.refused ?? 0, errors: s?.errors ?? 0, shadowHits: s?.shadow_hits ?? 0, daily: s?.daily ?? [] };
  };
  const stored = (policy?.plugins ?? []).map((p): GuardRow => ({ ...p, inPolicy: true, ...counts(p.name) }));
  const known = new Set(stored.map((r) => r.name));
  const extra = stats.plugins
    .filter((s) => !known.has(s.plugin))
    .map((s): GuardRow => ({
      name: s.plugin, tool: "", subcommand: [], action: "unknown", message: "", failClosed: false, builtin: "",
      inPolicy: false, ...counts(s.plugin),
    }));
  return [...stored, ...extra];
}
