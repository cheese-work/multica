import type { Agent, ChatSession } from "../types";

export const RECENT_AGENTS_WINDOW_MS = 3 * 24 * 60 * 60 * 1000;
export const RECENT_AGENTS_MAX = 3;

/**
 * Agents the user most often started a new DM with inside the window, best
 * first: DM count, then latest start, then name. Each id appears once. Forks and continuations are
 * not new DMs; archived sessions still count. Only `agents` are eligible.
 */
export function rankRecentAgents(
  sessions: readonly ChatSession[],
  agents: readonly Agent[],
  userId: string,
  now: number,
): string[] {
  const since = now - RECENT_AGENTS_WINDOW_MS;
  const stats = new Map<string, { count: number; latest: number }>();
  for (const s of sessions) {
    if (s.creator_id !== userId) continue;
    if (s.forked_from_session_id || s.continued_from_session_id) continue;
    const created = Date.parse(s.created_at);
    if (!(created >= since && created <= now)) continue;
    const prev = stats.get(s.agent_id);
    stats.set(s.agent_id, {
      count: (prev?.count ?? 0) + 1,
      latest: Math.max(prev?.latest ?? 0, created),
    });
  }
  const seen = new Set<string>();
  return agents
    .filter((a) => stats.has(a.id) && !seen.has(a.id) && seen.add(a.id))
    .sort((a, b) => {
      const sa = stats.get(a.id)!;
      const sb = stats.get(b.id)!;
      return sb.count - sa.count || sb.latest - sa.latest || a.name.localeCompare(b.name);
    })
    .slice(0, RECENT_AGENTS_MAX)
    .map((a) => a.id);
}
