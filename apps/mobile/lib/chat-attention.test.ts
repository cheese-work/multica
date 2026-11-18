import { describe, expect, it } from "vitest";
import type { ChatSession } from "@multica/core/types";
import {
  attentionRefreshDelay,
  attentionRowState,
  attentionAge,
  REFRESH_MIN_MS,
  REFRESH_SLACK_MS,
} from "./chat-attention";

function session(over: Partial<ChatSession>): ChatSession {
  return {
    id: "s1",
    workspace_id: "w",
    agent_id: "a",
    creator_id: "u",
    title: "t",
    status: "active",
    has_unread: false,
    created_at: "",
    updated_at: "",
    ...over,
  };
}

describe("attentionRowState", () => {
  it("is hidden when the server sent no attention fields (older server)", () => {
    expect(attentionRowState(session({}))).toBeNull();
  });

  it("is hidden for stale_level none", () => {
    expect(attentionRowState(session({ stale_level: "none", attention_keys: ["item:1"] }))).toBeNull();
  });

  it.each(["fresh", "yellow", "red"] as const)("shows level %s with source and since", (level) => {
    const row = attentionRowState(
      session({
        stale_level: level,
        attention_source: "ask",
        attention_since: "2026-11-13T00:00:00Z",
        attention_keys: ["item:1"],
      }),
    );
    expect(row).toMatchObject({ level, source: "ask", since: "2026-11-13T00:00:00Z" });
  });

  it("falls back to source ask when the server sends a level without a source", () => {
    expect(attentionRowState(session({ stale_level: "red", attention_keys: ["item:1"] }))?.source).toBe("ask");
  });

  it("never colours an unclassified-only session: no row (no colour, no age)", () => {
    expect(
      attentionRowState(session({ stale_level: "none", attention_unclassified_keys: ["turn:9"] })),
    ).toBeNull();
  });

  it("Xong drops decision keys; Để sau keeps every displayed key", () => {
    const row = attentionRowState(
      session({
        stale_level: "red",
        attention_source: "decision",
        attention_keys: ["decision:7@2", "item:1", "turn:3"],
      }),
    )!;
    expect(row.keys).toEqual(["decision:7@2", "item:1", "turn:3"]);
    expect(row.xongKeys).toEqual(["item:1", "turn:3"]);
    expect(row.canXong).toBe(true);
    expect(row.canLater).toBe(true);
  });

  it("decision-only row: no Xong, Để sau still allowed", () => {
    const row = attentionRowState(
      session({ stale_level: "yellow", attention_source: "decision", attention_keys: ["decision:7@2"] }),
    )!;
    expect(row.canXong).toBe(false);
    expect(row.canLater).toBe(true);
  });

  it("row without displayed keys has no actions", () => {
    const row = attentionRowState(session({ stale_level: "fresh", attention_source: "unseen" }))!;
    expect(row.canXong).toBe(false);
    expect(row.canLater).toBe(false);
  });
});

describe("attentionAge", () => {
  const now = Date.parse("2026-11-13T12:00:00Z");
  it("minutes under an hour", () => {
    expect(attentionAge("2026-11-13T11:30:00Z", now)).toEqual({ unit: "minutes", count: 30 });
  });
  it("hours under a day", () => {
    expect(attentionAge("2026-11-13T07:00:00Z", now)).toEqual({ unit: "hours", count: 5 });
  });
  it("days", () => {
    expect(attentionAge("2026-11-10T12:00:00Z", now)).toEqual({ unit: "days", count: 3 });
  });
  it("clamps the future to 0 minutes and rejects garbage", () => {
    expect(attentionAge("2026-11-13T13:00:00Z", now)).toEqual({ unit: "minutes", count: 0 });
    expect(attentionAge("nope", now)).toBeNull();
  });
});

describe("attentionRefreshDelay (A25: a due later reminder appears without reload)", () => {
  const now = Date.parse("2026-11-18T10:00:00Z");
  const at = (ms: number) => new Date(now + ms).toISOString();

  it("is null when no session schedules a change", () => {
    expect(attentionRefreshDelay([session({}), session({ next_change_at: null })], now)).toBeNull();
  });

  it("waits for the earliest next_change_at plus slack", () => {
    const sessions = [
      session({ id: "a", next_change_at: at(2 * 3600_000) }),
      session({ id: "b", next_change_at: at(600_000) }),
    ];
    expect(attentionRefreshDelay(sessions, now)).toBe(600_000 + REFRESH_SLACK_MS);
  });

  it("never fires sooner than the floor, so a stale instant cannot spin", () => {
    expect(attentionRefreshDelay([session({ next_change_at: at(-60_000) })], now)).toBe(REFRESH_MIN_MS);
  });

  it("ignores an unparseable time", () => {
    expect(attentionRefreshDelay([session({ next_change_at: "soon" })], now)).toBeNull();
  });

  it("caps at the timer limit", () => {
    expect(attentionRefreshDelay([session({ next_change_at: at(1e12) })], now)).toBe(2_147_483_647);
  });
});
