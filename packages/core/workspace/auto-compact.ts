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
