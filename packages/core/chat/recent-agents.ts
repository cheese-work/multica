import type { Agent, ChatSession } from "../types";

export const RECENT_AGENTS_WINDOW_MS = 3 * 24 * 60 * 60 * 1000;

// Inert stub: the tests are written against this before the real logic exists.
export function pickRecentAgentIds(
  _sessions: readonly ChatSession[],
  _agents: readonly Agent[],
  _userId: string,
  _now: number,
): string[] {
  return [];
}
