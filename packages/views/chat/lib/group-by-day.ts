import { sessionActivityTime } from "@multica/core/chat/queries";
import type { ChatSession } from "@multica/core/types";

export type DayGroupKey = "today" | "yesterday" | "thisWeek" | "older";

export interface DayGroup {
  key: DayGroupKey;
  sessions: ChatSession[];
}

const DAY_MS = 86_400_000;

/** Whole days since the epoch of the calendar day `ms` falls on in `tz`. */
function localDayNumber(ms: number, tz: string): number {
  const parts = new Intl.DateTimeFormat("en-US", {
    timeZone: tz,
    year: "numeric",
    month: "numeric",
    day: "numeric",
  }).formatToParts(ms);
  const get = (type: string) => Number(parts.find((p) => p.type === type)?.value);
  return Date.UTC(get("year"), get("month") - 1, get("day")) / DAY_MS;
}

/**
 * Buckets sessions by the local calendar day of their last activity: today,
 * yesterday, earlier this ISO week (Monday start), older. Always returns the
 * four groups in that order; input order is kept inside each group. A time in
 * the future counts as today.
 */
export function groupByDay(sessions: readonly ChatSession[], now: Date | number, tz: string): DayGroup[] {
  const today = localDayNumber(new Date(now).getTime(), tz);
  const weekStart = today - ((new Date(today * DAY_MS).getUTCDay() + 6) % 7);
  const groups: DayGroup[] = (["today", "yesterday", "thisWeek", "older"] as const).map((key) => ({
    key,
    sessions: [],
  }));
  const [todayG, yesterdayG, weekG, olderG] = groups as [DayGroup, DayGroup, DayGroup, DayGroup];
  for (const s of sessions) {
    const ms = sessionActivityTime(s);
    const day = Number.isNaN(ms) ? -Infinity : localDayNumber(ms, tz);
    if (day >= today) todayG.sessions.push(s);
    else if (day === today - 1) yesterdayG.sessions.push(s);
    else if (day >= weekStart) weekG.sessions.push(s);
    else olderG.sessions.push(s);
  }
  return groups;
}
