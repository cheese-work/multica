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

/**
 * Reads settings.auto_compact from the workspace settings blob. The server
 * validates on write, but the blob is untyped JSON: a missing or malformed
 * field falls back to its default so the UI never renders a value the server
 * would reject.
 */
export function readAutoCompact(
  settings: Record<string, unknown> | undefined,
): AutoCompactSettings {
  const raw = settings?.auto_compact;
  const saved =
    raw !== null && typeof raw === "object"
      ? (raw as Record<string, unknown>)
      : {};
  return {
    enabled: typeof saved.enabled === "boolean" ? saved.enabled : true,
    threshold_pct:
      typeof saved.threshold_pct === "number" &&
      isValidAutoCompactPct(saved.threshold_pct)
        ? saved.threshold_pct
        : AUTO_COMPACT_DEFAULT_PCT,
  };
}

/**
 * The effective rule for one DM, mirroring the server: the agent's
 * runtime_config.auto_compact overrides the workspace value field by field,
 * and a null or malformed override inherits.
 */
export function resolveAutoCompact(
  workspaceSettings: Record<string, unknown> | undefined,
  agentRuntimeConfig: Record<string, unknown> | undefined,
): AutoCompactSettings {
  const rule = readAutoCompact(workspaceSettings);
  const raw = agentRuntimeConfig?.auto_compact;
  if (raw === null || typeof raw !== "object") return rule;
  const override = raw as Record<string, unknown>;
  return {
    enabled: typeof override.enabled === "boolean" ? override.enabled : rule.enabled,
    threshold_pct:
      typeof override.threshold_pct === "number" && isValidAutoCompactPct(override.threshold_pct)
        ? override.threshold_pct
        : rule.threshold_pct,
  };
}
