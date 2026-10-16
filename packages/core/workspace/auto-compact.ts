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
