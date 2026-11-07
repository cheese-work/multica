// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { Agent, ChatSession } from "../types";
import { RECENT_AGENTS_MAX, RECENT_AGENTS_WINDOW_MS, rankRecentAgents } from "./recent-agents";

const NOW = Date.parse("2026-11-02T12:00:00Z");
const DAY = 24 * 60 * 60 * 1000;
const ME = "user-me";

const agent = (id: string, name = id) => ({ id, name }) as Agent;
const session = (over: Partial<ChatSession> & { id: string; agent_id: string }): ChatSession =>
  ({
    creator_id: ME,
    status: "active",
    created_at: new Date(NOW - DAY).toISOString(),
    ...over,
  }) as ChatSession;

const AGENTS = [agent("a"), agent("b"), agent("c"), agent("d")];

describe("rankRecentAgents", () => {
  it("uses a 3-day window and max 3", () => {
    expect(RECENT_AGENTS_WINDOW_MS).toBe(3 * DAY);
    expect(RECENT_AGENTS_MAX).toBe(3);
  });

  it("counts a session exactly at the 3-day boundary, drops one just outside", () => {
    const sessions = [
      session({ id: "1", agent_id: "a", created_at: new Date(NOW - 3 * DAY).toISOString() }),
      session({ id: "2", agent_id: "b", created_at: new Date(NOW - 3 * DAY - 1).toISOString() }),
    ];
    expect(rankRecentAgents(sessions, AGENTS, ME, NOW)).toEqual(["a"]);
  });

  it("ignores sessions dated after now, counts one at now", () => {
    const sessions = [
      session({ id: "1", agent_id: "a", created_at: new Date(NOW + 1).toISOString() }),
      session({ id: "2", agent_id: "b", created_at: new Date(NOW).toISOString() }),
    ];
    expect(rankRecentAgents(sessions, AGENTS, ME, NOW)).toEqual(["b"]);
  });

  it("ignores sessions created by other users", () => {
    const sessions = [session({ id: "1", agent_id: "a", creator_id: "someone-else" })];
    expect(rankRecentAgents(sessions, AGENTS, ME, NOW)).toEqual([]);
  });

  it("ignores forks and continuations", () => {
    const sessions = [
      session({ id: "1", agent_id: "a", forked_from_session_id: "x" }),
      session({ id: "2", agent_id: "b", continued_from_session_id: "y" }),
      session({ id: "3", agent_id: "c", forked_from_session_id: null, continued_from_session_id: null }),
    ];
    expect(rankRecentAgents(sessions, AGENTS, ME, NOW)).toEqual(["c"]);
  });

  it("counts archived sessions", () => {
    const sessions = [session({ id: "1", agent_id: "a", status: "archived" })];
    expect(rankRecentAgents(sessions, AGENTS, ME, NOW)).toEqual(["a"]);
  });

  it("orders by count, then latest created_at, then name", () => {
    const at = (h: number) => new Date(NOW - h * 3600_000).toISOString();
    const sessions = [
      session({ id: "1", agent_id: "a", created_at: at(10) }),
      session({ id: "2", agent_id: "a", created_at: at(11) }),
      session({ id: "3", agent_id: "b", created_at: at(5) }),
      session({ id: "4", agent_id: "c", created_at: at(2) }),
    ];
    // a: 2 DMs first; c newer than b on the 1-DM tie.
    expect(rankRecentAgents(sessions, AGENTS, ME, NOW)).toEqual(["a", "c", "b"]);

    const sameTime = [
      session({ id: "5", agent_id: "b", created_at: at(5) }),
      session({ id: "6", agent_id: "a", created_at: at(5) }),
    ];
    expect(rankRecentAgents(sameTime, AGENTS, ME, NOW)).toEqual(["a", "b"]);
  });

  it("returns at most 3 agents", () => {
    const sessions = AGENTS.map((a, i) =>
      session({ id: String(i), agent_id: a.id, created_at: new Date(NOW - (i + 1) * 3600_000).toISOString() }),
    );
    expect(rankRecentAgents(sessions, AGENTS, ME, NOW)).toEqual(["a", "b", "c"]);
  });

  it("drops agents missing from the eligible list", () => {
    const sessions = [session({ id: "1", agent_id: "gone" }), session({ id: "2", agent_id: "a" })];
    expect(rankRecentAgents(sessions, AGENTS, ME, NOW)).toEqual(["a"]);
  });

  it("returns an empty list when nothing qualifies", () => {
    expect(rankRecentAgents([], AGENTS, ME, NOW)).toEqual([]);
    expect(rankRecentAgents([session({ id: "1", agent_id: "a" })], [], ME, NOW)).toEqual([]);
  });
});
