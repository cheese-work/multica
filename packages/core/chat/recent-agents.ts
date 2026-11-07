import type { Agent, ChatSession } from "../types";

export const RECENT_AGENTS_WINDOW_MS = 3 * 24 * 60 * 60 * 1000;
const MAX_RECENT_AGENTS = 3;

interface DmTally {
  count: number;
  latest: number;
}

/**
 * Ids of the (up to 3) agents the user started the most DMs with in the last
 * 3 days. Order: DM count, then most recent DM, then agent name. Forks and
 * continuations are not new DMs; archived DMs still count. Only agents in
 * `agents` are returned, each once.
 */
export function pickRecentAgentIds(
  sessions: readonly ChatSession[],
  agents: readonly Agent[],
  userId: string,
  now: number,
): string[] {
  const windowStart = now - RECENT_AGENTS_WINDOW_MS;
  const tallies = new Map<string, DmTally>();

  for (const session of sessions) {
    if (session.creator_id !== userId) continue;
    if (session.forked_from_session_id || session.continued_from_session_id) continue;
    const startedAt = Date.parse(session.created_at);
    if (!(startedAt >= windowStart && startedAt <= now)) continue;
    const tally = tallies.get(session.agent_id) ?? { count: 0, latest: startedAt };
    tally.count += 1;
    tally.latest = Math.max(tally.latest, startedAt);
    tallies.set(session.agent_id, tally);
  }

  const candidates = new Map<string, Agent>();
  for (const agent of agents) {
    if (tallies.has(agent.id) && !candidates.has(agent.id)) candidates.set(agent.id, agent);
  }

  return [...candidates.values()]
    .sort((a, b) => {
      const ta = tallies.get(a.id)!;
      const tb = tallies.get(b.id)!;
      return tb.count - ta.count || tb.latest - ta.latest || a.name.localeCompare(b.name);
    })
    .slice(0, MAX_RECENT_AGENTS)
    .map((agent) => agent.id);
}
