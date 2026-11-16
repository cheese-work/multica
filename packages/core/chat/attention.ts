// Pure helpers for the DM attention actions (Xong / Để sau / Xong tất cả, /later).
// Subject keys are opaque server strings: `item:<id>`, `decision:<id>@<rev>`, `turn:<task_id>`.
// The client sends exactly the keys it displayed (plan r4, snapshot contract).

export const LATER_MAX_SECONDS = 7 * 86400;

const UNIT_SECONDS = { s: 1, m: 60, h: 3600, d: 86400 } as const;

/**
 * Parses the `/later [duration]` argument. Empty → server default (2 h).
 * Accepts `<n>s|m|h|d`, 1 s up to 7 d.
 */
export function parseLaterDuration(raw: string): { ok: true; seconds: number | undefined } | { ok: false } {
  const text = raw.trim().toLowerCase();
  if (!text) return { ok: true, seconds: undefined };
  const m = /^(\d+)([smhd])$/.exec(text);
  if (!m) return { ok: false };
  const seconds = Number(m[1]) * UNIT_SECONDS[m[2] as keyof typeof UNIT_SECONDS];
  if (seconds < 1 || seconds > LATER_MAX_SECONDS) return { ok: false };
  return { ok: true, seconds };
}

export function isDecisionKey(key: string): boolean {
  return key.startsWith("decision:");
}

/** Keys Xong / Để sau may act on: decisions are answered, never acked or snoozed (T3, T4). */
export function snoozableKeys(keys: readonly string[]): string[] {
  return keys.filter((k) => !isDecisionKey(k));
}
