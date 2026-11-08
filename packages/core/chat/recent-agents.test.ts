// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { Agent, ChatSession } from "../types";
import { RECENT_AGENTS_WINDOW_MS, pickRecentAgentIds } from "./recent-agents";

const HOUR = 60 * 60 * 1000;
const NOW = Date.parse("2026-03-10T09:00:00Z");
const ME = "me";

const agent = (id: string, name: string): Agent => ({ id, name }) as Agent;
const dm = (agentId: string, hoursAgo: number, extra: Partial<ChatSession> = {}): ChatSession =>
  ({
    id: `${agentId}-${hoursAgo}-${Math.random()}`,
    agent_id: agentId,
    creator_id: ME,
    status: "active",
    created_at: new Date(NOW - hoursAgo * HOUR).toISOString(),
    ...extra,
  }) as ChatSession;

const pick = (sessions: ChatSession[], agents: Agent[], userId = ME) =>
  pickRecentAgentIds(sessions, agents, userId, NOW);

const ALL = [agent("alpha", "Alpha"), agent("beta", "Beta"), agent("gamma", "Gamma"), agent("delta", "Delta")];

describe("pickRecentAgentIds", () => {
  it("looks back exactly three days", () => {
    expect(RECENT_AGENTS_WINDOW_MS).toBe(72 * HOUR);
  });

  it("includes a DM started exactly three days ago and excludes one a millisecond older", () => {
    const edge = dm("alpha", 72);
    const outside = { ...dm("beta", 72), created_at: new Date(NOW - 72 * HOUR - 1).toISOString() };
    expect(pick([edge, outside], ALL)).toEqual(["alpha"]);
  });

  it("ignores DMs dated after now", () => {
    const future = { ...dm("alpha", 0), created_at: new Date(NOW + 1).toISOString() };
    expect(pick([future, dm("beta", 1)], ALL)).toEqual(["beta"]);
  });

  it("only counts DMs the given user created", () => {
    expect(pick([dm("alpha", 1, { creator_id: "someone-else" }), dm("beta", 2)], ALL)).toEqual(["beta"]);
  });

  it("does not count forks or continuations as new DMs", () => {
    const sessions = [
      dm("alpha", 1, { forked_from_session_id: "s0" }),
      dm("beta", 1, { continued_from_session_id: "s0" }),
      dm("gamma", 1),
    ];
    expect(pick(sessions, ALL)).toEqual(["gamma"]);
  });

  it("counts archived DMs", () => {
    expect(pick([dm("alpha", 5, { status: "archived" })], ALL)).toEqual(["alpha"]);
  });

  it("ranks by DM count first", () => {
    const sessions = [dm("alpha", 1), dm("beta", 30), dm("beta", 31)];
    expect(pick(sessions, ALL)).toEqual(["beta", "alpha"]);
  });

  it("breaks a count tie by the most recent DM", () => {
    const sessions = [dm("alpha", 20), dm("beta", 2)];
    expect(pick(sessions, ALL)).toEqual(["beta", "alpha"]);
  });

  it("breaks a full tie by name", () => {
    const sessions = [dm("gamma", 4), dm("alpha", 4), dm("beta", 4)];
    expect(pick(sessions, ALL)).toEqual(["alpha", "beta", "gamma"]);
  });

  it("returns at most three agents", () => {
    const sessions = ALL.map((a, i) => dm(a.id, i + 1));
    expect(pick(sessions, ALL)).toHaveLength(3);
  });

  it("drops agents that are not in the agent list", () => {
    expect(pick([dm("ghost", 1), dm("alpha", 2)], ALL)).toEqual(["alpha"]);
  });

  it("lists an agent once even if the agent list repeats it", () => {
    const repeated = [agent("alpha", "Alpha"), agent("beta", "Beta"), agent("alpha", "Alpha")];
    expect(pick([dm("alpha", 1), dm("beta", 2)], repeated)).toEqual(["alpha", "beta"]);
  });

  it("returns an empty list when nothing qualifies", () => {
    expect(pick([], ALL)).toEqual([]);
    expect(pick([dm("alpha", 1)], [])).toEqual([]);
  });
});
