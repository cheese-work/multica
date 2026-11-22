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

/** Wait past the server's instant so the refetch lands after the change. */
export const REFRESH_SLACK_MS = 500;
/** Floor for a refetch timer, so a stale next_change_at cannot spin. */
export const REFRESH_MIN_MS = 5_000;
/** setTimeout overflows past 2^31-1 ms; an early refetch is harmless. */
export const REFRESH_MAX_MS = 2_147_483_647;

/** Earliest `next_change_at` across the sessions (epoch ms), or null when none is scheduled. */
export function nextAttentionChange(sessions: ChatSession[]): number | null {
  const times = sessions.map((s) => Date.parse(s.next_change_at ?? "")).filter(Number.isFinite);
  return times.length === 0 ? null : Math.min(...times);
}

/**
 * Milliseconds until the attention projection next changes (a later reminder
 * falling due, a 4 h or 24 h crossing), or null when nothing is scheduled.
 * The sessions cache never goes stale on its own, so the screen refetches then.
 */
export function attentionRefreshDelay(sessions: ChatSession[], now: number): number | null {
  const next = nextAttentionChange(sessions);
  if (next === null) return null;
  return Math.min(Math.max(next - now + REFRESH_SLACK_MS, REFRESH_MIN_MS), REFRESH_MAX_MS);
}
