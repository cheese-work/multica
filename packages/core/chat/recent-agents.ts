import type { Agent, ChatSession } from "../types";

export const RECENT_AGENTS_WINDOW_MS = 3 * 24 * 60 * 60 * 1000;
export const RECENT_AGENTS_MAX = 3;

// STUB (test-first red step): inert until the tests exist and fail.
export function rankRecentAgents(
  _sessions: readonly ChatSession[],
  _agents: readonly Agent[],
  _userId: string,
  _now: number,
): string[] {
  return [];
}
