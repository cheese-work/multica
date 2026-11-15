/**
 * Pure state for the DM attention row (plan r4). Mobile mirrors the web rule
 * set without importing web hooks. Subject keys are opaque server strings;
 * the client sends back exactly the keys it displayed.
 */
import type {
  ChatAttentionSource,
  ChatSession,
  ChatStaleLevel,
} from "@multica/core/types";
import { snoozableKeys } from "@multica/core/chat/attention";

export type AttentionLevel = Exclude<ChatStaleLevel, "none">;

export interface AttentionRowState {
  level: AttentionLevel;
  source: ChatAttentionSource;
  since: string | null;
  /** Every displayed key: the Để sau snapshot. */
  keys: string[];
  /** Displayed keys minus `decision:` keys: the Xong snapshot (T3). */
  xongKeys: string[];
  canXong: boolean;
  canLater: boolean;
}

/**
 * Row state, or null when the session owes Cheese nothing: older server (no
 * fields), `none`, and unclassified-only turns, which get no colour and no age.
 */
export function attentionRowState(s: ChatSession): AttentionRowState | null {
  const level = s.stale_level;
  if (level !== "fresh" && level !== "yellow" && level !== "red") return null;
  const keys = s.attention_keys ?? [];
  const xongKeys = snoozableKeys(keys);
  return {
    level,
    source: s.attention_source ?? "ask",
    since: s.attention_since ?? null,
    keys,
    xongKeys,
    canXong: xongKeys.length > 0,
    canLater: keys.length > 0,
  };
}

export type AttentionAge = { unit: "minutes" | "hours" | "days"; count: number };

/** Whole minutes, hours or days since `since`; null for an unparseable time. */
export function attentionAge(since: string, now: number): AttentionAge | null {
  const elapsed = now - Date.parse(since);
  if (!Number.isFinite(elapsed)) return null;
  const minutes = Math.max(0, Math.floor(elapsed / 60_000));
  if (minutes < 60) return { unit: "minutes", count: minutes };
  if (minutes < 1440) return { unit: "hours", count: Math.floor(minutes / 60) };
  return { unit: "days", count: Math.floor(minutes / 1440) };
}
