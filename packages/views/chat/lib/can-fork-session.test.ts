import { describe, expect, it } from "vitest";
import type { ChatSession } from "@multica/core/types";
import { canForkSession } from "./can-fork-session";

const session = (overrides: Partial<ChatSession> = {}): ChatSession => ({
  id: "s1", workspace_id: "ws", agent_id: "a", creator_id: "u", title: "", status: "active",
  has_unread: false, created_at: "2026-10-13T00:00:00Z", updated_at: "2026-10-13T00:00:00Z",
  ...overrides,
});

describe("canForkSession", () => {
  it("allows a saved, active web DM", () => {
    expect(canForkSession(session())).toBe(true);
  });

  it("denies a draft (no session yet)", () => {
    expect(canForkSession(null)).toBe(false);
    expect(canForkSession(undefined)).toBe(false);
  });

  it("denies an archived DM", () => {
    expect(canForkSession(session({ status: "archived" }))).toBe(false);
  });

  it("denies a channel-sourced DM", () => {
    expect(canForkSession(session({
      channel_source: { channel_type: "slack", installation_id: "i1", route_revision: 1 },
    }))).toBe(false);
  });
});
