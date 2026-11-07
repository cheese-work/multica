import { z } from "zod";

export const AUTO_COMPACT_MIN_PCT = 50;
export const AUTO_COMPACT_MAX_PCT = 90;
const AUTO_COMPACT_DEFAULT_PCT = 80;

export interface AutoCompactSettings {
  enabled: boolean;
  threshold_pct: number;
}

export function isValidAutoCompactPct(value: number): boolean {
  return (
    Number.isInteger(value) &&
    value >= AUTO_COMPACT_MIN_PCT &&
    value <= AUTO_COMPACT_MAX_PCT
  );
}

const DEFAULT_AUTO_COMPACT: AutoCompactSettings = {
  enabled: true,
  threshold_pct: AUTO_COMPACT_DEFAULT_PCT,
};

/**
 * Lenient schema for settings.auto_compact. The server validates on write,
 * but the settings blob is untyped JSON: each malformed field falls back to
 * its own default and a non-object value falls back to all defaults, so the
 * UI never renders a value the server would reject.
 */
export const AutoCompactSchema = z
  .object({
    enabled: z.boolean().catch(DEFAULT_AUTO_COMPACT.enabled),
    threshold_pct: z
      .number()
      .refine(isValidAutoCompactPct)
      .catch(DEFAULT_AUTO_COMPACT.threshold_pct),
  })
  .catch(DEFAULT_AUTO_COMPACT);

export function readAutoCompact(
  settings: Record<string, unknown> | undefined,
): AutoCompactSettings {
  return AutoCompactSchema.parse(settings?.auto_compact);
}

// CHE-4331: with no percent saved, the trigger is a token count chosen by the
// model's window. Mirrors server/internal/service/auto_compact.go.
const SMALL_WINDOW = 200_000;
export const AUTO_COMPACT_MIN_TOKENS = 100_000;
export const AUTO_COMPACT_MAX_TOKENS_200K = 200_000;
export const AUTO_COMPACT_MAX_TOKENS_1M = 400_000;
const MIN_TOKENS = AUTO_COMPACT_MIN_TOKENS;
const MAX_TOKENS_200K = AUTO_COMPACT_MAX_TOKENS_200K;
const MAX_TOKENS_1M = AUTO_COMPACT_MAX_TOKENS_1M;

export interface ResolvedAutoCompact {
  enabled: boolean;
  /** Saved percent of the window, or null to use the per-window token counts. */
  threshold_pct: number | null;
  tokens_200k: number;
  tokens_1m: number;
}

export function isValidAutoCompactTokens(value: unknown, max: number): value is number {
  return typeof value === "number" && Number.isInteger(value) && value >= MIN_TOKENS && value <= max;
}

/**
 * The effective rule for one DM, mirroring the server: the agent's
 * runtime_config.auto_compact overrides the workspace value field by field,
 * and a null or malformed override inherits. A layer that names a token field
 * switches back to tokens unless it also names a percent.
 */
export function resolveAutoCompact(
  workspaceSettings: Record<string, unknown> | undefined,
  agentRuntimeConfig: Record<string, unknown> | undefined,
): ResolvedAutoCompact {
  const rule: ResolvedAutoCompact = { enabled: true, threshold_pct: null, tokens_200k: 190_000, tokens_1m: 200_000 };
  for (const layer of [workspaceSettings, agentRuntimeConfig]) {
    const raw = layer?.auto_compact;
    if (raw === null || typeof raw !== "object" || Array.isArray(raw)) continue;
    const o = raw as Record<string, unknown>;
    if (typeof o.enabled === "boolean") rule.enabled = o.enabled;
    let tokenSet = false;
    if (isValidAutoCompactTokens(o.threshold_tokens_200k, MAX_TOKENS_200K)) {
      rule.tokens_200k = o.threshold_tokens_200k;
      tokenSet = true;
    }
    if (isValidAutoCompactTokens(o.threshold_tokens_1m, MAX_TOKENS_1M)) {
      rule.tokens_1m = o.threshold_tokens_1m;
      tokenSet = true;
    }
    if (tokenSet) rule.threshold_pct = null;
    if (typeof o.threshold_pct === "number" && isValidAutoCompactPct(o.threshold_pct)) {
      rule.threshold_pct = o.threshold_pct;
    }
  }
  return rule;
}

/**
 * The prompt size at which this DM compacts, or null when auto-compact is off
 * or the window is unknown. Mirrors ThresholdTokens in the server: a saved
 * percent rounds up; otherwise the window's token count, capped at 95% of a
 * smaller window.
 */
export function autoCompactThresholdTokens(
  rule: ResolvedAutoCompact,
  window: number | null | undefined,
): number | null {
  if (!rule.enabled || !window || window <= 0) return null;
  if (rule.threshold_pct !== null) return Math.ceil((rule.threshold_pct * window) / 100);
  const tokens = window <= SMALL_WINDOW ? rule.tokens_200k : rule.tokens_1m;
  return Math.min(tokens, Math.floor((window * 95) / 100));
}
