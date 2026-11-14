import type { GuardPluginStats, GuardStats } from "@multica/core/types";

export type GuardAction = "fix" | "deny" | "shadow" | "pass" | "unknown";

/** Where a rule is stored: the workspace policy itself or one of its org, repo or path layers. */
export type GuardScope = "workspace" | "org" | "repo" | "path";

export interface GuardPolicyPlugin {
  /** Unique per stored entry; a plugin name can repeat across scopes. */
  key: string;
  scope: GuardScope;
  /** Org, repo or repo/path the layer covers; empty for the workspace. */
  target: string;
  layerLocked: boolean;
  name: string;
  tool: string;
  subcommand: string[];
  action: GuardAction;
  message: string;
  failClosed: boolean;
  builtin: string;
}

export interface GuardReviewRule {
  scope: GuardScope;
  target: string;
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
  /** True when another stored entry has the same name: hit counts are per name, not per scope. */
  sharedCounts: boolean;
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

const SCOPES = ["org", "repo", "path"] as const;

function layerTarget(scope: GuardScope, layer: Record<string, unknown>): string {
  if (scope === "org") return text(layer.org);
  if (scope === "repo") return text(layer.repo);
  if (scope === "path") return [text(layer.repo), text(layer.path).replace(/^\/+|\/+$/g, "")].filter(Boolean).join("/");
  return "";
}

function readPlugins(raw: unknown, scope: GuardScope, target: string, layerLocked: boolean, at: number): GuardPolicyPlugin[] {
  return (Array.isArray(raw) ? raw : []).flatMap((item, i): GuardPolicyPlugin[] => {
    const plugin = record(item);
    const name = text(plugin?.name);
    if (!plugin || !name) return [];
    const match = record(plugin.match);
    return [{
      key: `${at}:${i}:${name}`,
      scope,
      target,
      layerLocked,
      name,
      tool: text(match?.tool),
      subcommand: textList(match?.subcommand),
      action: ACTIONS.find((a) => a === plugin.action) ?? "unknown",
      message: text(plugin.message),
      failClosed: plugin.fail_closed === true,
      builtin: text(plugin.builtin),
    }];
  });
}

function readReview(raw: unknown, scope: GuardScope, target: string): GuardReviewRule[] {
  return (Array.isArray(raw) ? raw : []).flatMap((item): GuardReviewRule[] => {
    const rule = record(item);
    const repo = text(rule?.repo);
    return rule && repo ? [{ scope, target, repo, github: text(rule.github), reviewers: textList(rule.reviewers) }] : [];
  });
}

/** Null when the workspace stores no policy (the built-in rules run) or the value is not an object. */
export function parseGuardPolicy(settings: unknown): GuardPolicyView | null {
  const policy = record(record(settings)?.guard_policy);
  if (!policy) return null;
  const plugins = readPlugins(policy.plugins, "workspace", "", false, 0);
  const review = readReview(policy.review, "workspace", "");
  (Array.isArray(policy.layers) ? policy.layers : []).forEach((raw, i) => {
    const layer = record(raw);
    const scope = SCOPES.find((s) => s === layer?.scope);
    if (!layer || !scope) return;
    const target = layerTarget(scope, layer);
    plugins.push(...readPlugins(layer.plugins, scope, target, layer.locked === true, i + 1));
    review.push(...readReview(layer.review, scope, target));
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
  const uses = new Map<string, number>();
  for (const p of policy?.plugins ?? []) uses.set(p.name, (uses.get(p.name) ?? 0) + 1);
  const stored = (policy?.plugins ?? []).map((p): GuardRow => ({ ...p, inPolicy: true, sharedCounts: (uses.get(p.name) ?? 0) > 1, ...counts(p.name) }));
  const known = new Set(stored.map((r) => r.name));
  const extra = stats.plugins
    .filter((s) => !known.has(s.plugin))
    .map((s): GuardRow => ({
      key: `hit:${s.plugin}`, scope: "workspace", target: "", layerLocked: false,
      name: s.plugin, tool: "", subcommand: [], action: "unknown", message: "", failClosed: false, builtin: "",
      inPolicy: false, sharedCounts: false, ...counts(s.plugin),
    }));
  return [...stored, ...extra];
}
