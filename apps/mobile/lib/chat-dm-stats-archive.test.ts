import { describe, expect, it, vi } from "vitest";
import type { ChatSession, ChatSessionUsage } from "@/data/schemas";
import {
  useChatSessionActionsStore,
} from "@/data/stores/chat-session-actions-store";
import {
  ArchivedSendInterceptedError,
  buildArchivedSendAlertButtons,
  buildChatMoreMenuItems,
  deriveChatStatisticsState,
  formatIctTime,
  formatStatsDuration,
  isArchivedSendIntercepted,
  patchSessionArchived,
  resolveRelatedSessions,
  splitChatSessions,
  unarchiveThenSendOnce,
} from "./chat-dm-stats-archive";

function makeSession(overrides: Partial<ChatSession> & { id: string }): ChatSession {
  return {
    workspace_id: "ws-1",
    creator_id: "user-1",
    agent_id: "agent-1",
    title: `Session ${overrides.id}`,
    status: "active",
    has_unread: false,
    created_at: "2026-10-21T01:00:00Z",
    updated_at: "2026-10-21T01:00:00Z",
    ...overrides,
  };
}

function makeUsage(overrides: Partial<ChatSessionUsage> = {}): ChatSessionUsage {
  return {
    session_id: "sess-1",
    tokens: {
      input: 1_000_000,
      output: 100_000,
      cache_read: 0,
      cache_write: 0,
    },
    uncosted_tokens: {
      input: 1_000_000,
      output: 100_000,
      cache_read: 0,
      cache_write: 0,
    },
    cost_usd_ticks: 0,
    runs: {
      by_status: { completed: 2 },
      terminal: 2,
      metered: 2,
      unreported: 0,
      total_run_ms: 65_000,
    },
    messages: {
      by_role: { user: 2, assistant: 2 },
    },
    by_model: {
      "claude-sonnet-4-5": {
        tokens: {
          input: 1_000_000,
          output: 100_000,
          cache_read: 0,
          cache_write: 0,
        },
        cost_usd_ticks: 0,
      },
    },
    continued_from_session_id: null,
    continued_into_session_id: null,
    forked_from_session_id: null,
    ...overrides,
  };
}

describe("splitChatSessions", () => {
  it("sorts active sessions with pinned first by activity, and archived sessions by activity only ignoring pinned", () => {
    const sessions: ChatSession[] = [
      makeSession({
        id: "active-newer-unpinned",
        status: "active",
        pinned: false,
        updated_at: "2026-10-21T05:00:00Z",
      }),
      makeSession({
        id: "active-older-pinned",
        status: "active",
        pinned: true,
        updated_at: "2026-10-21T02:00:00Z",
      }),
      makeSession({
        id: "archived-older-pinned",
        status: "archived",
        pinned: true,
        updated_at: "2026-10-21T03:00:00Z",
      }),
      makeSession({
        id: "archived-newer-unpinned",
        status: "archived",
        pinned: false,
        updated_at: "2026-10-21T06:00:00Z",
      }),
    ];

    const { active, archived } = splitChatSessions(sessions);

    expect(active.map((s) => s.id)).toEqual([
      "active-older-pinned",
      "active-newer-unpinned",
    ]);
    // Archived section ignores pinned and sorts strictly by activity desc
    expect(archived.map((s) => s.id)).toEqual([
      "archived-newer-unpinned",
      "archived-older-pinned",
    ]);
  });
});

describe("buildChatMoreMenuItems", () => {
  it("shows Statistics, Pin/Unpin, and Archive on an active DM, and hides Delete", () => {
    const items = buildChatMoreMenuItems({
      session: makeSession({ id: "s1", status: "active", pinned: false }),
      actionPending: null,
    });

    expect(items.map((i) => i.testID)).toEqual([
      "mobile-chat-more-statistics",
      "mobile-chat-more-pin",
      "mobile-chat-more-archive",
    ]);
    expect(items.find((i) => i.testID === "mobile-chat-more-delete")).toBeUndefined();
    expect(items.find((i) => i.testID === "mobile-chat-more-archive")?.disabled).toBe(false);
  });

  it("disables Archive row with pending state while archive mutation is in flight", () => {
    const items = buildChatMoreMenuItems({
      session: makeSession({ id: "s1", status: "active", pinned: true }),
      actionPending: "archive",
    });

    const archiveItem = items.find((i) => i.testID === "mobile-chat-more-archive");
    expect(archiveItem?.disabled).toBe(true);
    expect(archiveItem?.isPending).toBe(true);
  });

  it("hides Pin on an archived DM and includes Unarchive plus destructive Delete", () => {
    const items = buildChatMoreMenuItems({
      session: makeSession({ id: "s2", status: "archived", pinned: true }),
      actionPending: null,
    });

    expect(items.map((i) => i.testID)).toEqual([
      "mobile-chat-more-statistics",
      "mobile-chat-more-archive",
      "mobile-chat-more-delete",
    ]);
    expect(items.find((i) => i.testID === "mobile-chat-more-pin")).toBeUndefined();
    expect(items.find((i) => i.testID === "mobile-chat-more-delete")?.destructive).toBe(true);
  });
});

describe("deriveChatStatisticsState", () => {
  it("returns loading state before initial fetch resolves", () => {
    const state = deriveChatStatisticsState({
      sessionId: "sess-1",
      data: undefined,
      isFetched: false,
      isError: false,
      error: null,
      dataUpdatedAt: 0,
    });

    expect(state.kind).toBe("loading");
    expect(state.stateTestId).toBe("mobile-stats-state-loading");
  });

  it("returns error state on malformed/drift fallback payload (session_id mismatch) and never renders $0", () => {
    const state = deriveChatStatisticsState({
      sessionId: "sess-1",
      data: makeUsage({ session_id: "" }),
      isFetched: true,
      isError: false,
      error: null,
      dataUpdatedAt: Date.now(),
    });

    expect(state.kind).toBe("error");
    expect(state.stateTestId).toBe("mobile-stats-state-error");
    expect(state.summary).toBeNull();
  });

  it("returns empty state with em-dash cost when session has 0 total runs", () => {
    const state = deriveChatStatisticsState({
      sessionId: "sess-1",
      data: makeUsage({
        runs: {
          by_status: {},
          terminal: 0,
          metered: 0,
          unreported: 0,
          total_run_ms: 0,
        },
        tokens: { input: 0, output: 0, cache_read: 0, cache_write: 0 },
        uncosted_tokens: { input: 0, output: 0, cache_read: 0, cache_write: 0 },
        by_model: {},
      }),
      isFetched: true,
      isError: false,
      error: null,
      dataUpdatedAt: Date.now(),
    });

    expect(state.kind).toBe("empty");
    expect(state.stateTestId).toBe("mobile-stats-state-empty");
    expect(state.summary?.costUsd).toBeNull();
    expect(state.costDisplay).toBe("—");
  });

  it("returns populated state with estimated cost and ICT-formatted offline banner when background refresh fails", () => {
    // 2026-10-21T07:32:00Z === 14:32 ICT
    const updatedAt = Date.parse("2026-10-21T07:32:00Z");
    const state = deriveChatStatisticsState({
      sessionId: "sess-1",
      data: makeUsage({
        runs: {
          by_status: { completed: 2 },
          terminal: 2,
          metered: 1,
          unreported: 1,
          total_run_ms: 65_000,
        },
      }),
      isFetched: true,
      isError: true,
      error: new Error("Network down"),
      dataUpdatedAt: updatedAt,
    });

    expect(state.kind).toBe("populated");
    expect(state.refreshFailed).toBe(true);
    expect(state.offlineTestId).toBe("mobile-stats-state-offline");
    expect(state.updatedAtIct).toBe("14:32 ICT");
    expect(state.summary?.partial).toBe(true);
    expect(state.costDisplay).toBe("$4.50");
  });
});

describe("useChatSessionActionsStore", () => {
  it("guards concurrent archive/unarchive per session and manages 5s undo toast", () => {
    vi.useFakeTimers();
    try {
      const store = useChatSessionActionsStore.getState();
      store.reset();

      expect(store.beginSessionAction("sess-1", "archive")).toBe(true);
      // Duplicate call on the same session is rejected while in flight
      expect(store.beginSessionAction("sess-1", "archive")).toBe(false);
      // Different session is allowed independently
      expect(store.beginSessionAction("sess-2", "unarchive")).toBe(true);

      store.endSessionAction("sess-1");
      store.endSessionAction("sess-2");
      expect(useChatSessionActionsStore.getState().pendingBySession).toEqual({});

      store.showArchiveUndo({ sessionId: "sess-1", title: "Release prep" });
      expect(useChatSessionActionsStore.getState().undoToast?.sessionId).toBe("sess-1");

      vi.advanceTimersByTime(5_000);
      expect(useChatSessionActionsStore.getState().undoToast).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("ArchivedSendInterceptedError & helpers", () => {
  it("recognizes ArchivedSendInterceptedError so MessageComposer restores draft intact", () => {
    const err = new ArchivedSendInterceptedError();
    expect(isArchivedSendIntercepted(err)).toBe(true);
    expect(isArchivedSendIntercepted(new Error("other"))).toBe(false);
  });

  it("resolves related DM lineage titles with fallback when target session is not in cache", () => {
    const current = makeSession({
      id: "sess-current",
      continued_from_session_id: "sess-prev",
      continued_into_session_id: "sess-next",
      forked_from_session_id: "sess-missing",
    });
    const sessions = [
      current,
      makeSession({ id: "sess-prev", title: "Part 1" }),
      makeSession({ id: "sess-next", title: "Part 3" }),
    ];

    const related = resolveRelatedSessions(current, sessions, "Previous DM");
    expect(related).toEqual([
      { kind: "continued_from", id: "sess-prev", title: "Part 1" },
      { kind: "continued_into", id: "sess-next", title: "Part 3" },
      { kind: "forked_from", id: "sess-missing", title: "Previous DM" },
    ]);
  });

  it("formats durations and ICT timestamps", () => {
    expect(formatStatsDuration(0)).toBe("0s");
    expect(formatStatsDuration(45_000)).toBe("45s");
    expect(formatStatsDuration(125_000)).toBe("2m 5s");
    expect(formatIctTime(Date.parse("2026-10-21T07:05:00Z"))).toBe("14:05 ICT");
  });
});

describe("archived-send alert (A2)", () => {
  const labels = { openNewer: "Open the newer DM", confirm: "Unarchive and send", cancel: "Cancel" };
  const build = (platform: string, hasNewer: boolean) =>
    buildArchivedSendAlertButtons({
      platform,
      hasNewer,
      labels,
      onOpenNewer: vi.fn(),
      onConfirm: vi.fn(),
      onCancel: vi.fn(),
    });

  it("android with a newer DM: Open newer is positive (last), Unarchive negative, Cancel neutral (first)", () => {
    // RN Android maps buttons by array order: [neutral, negative, positive].
    expect(build("android", true).map((b) => b.text)).toEqual([
      "Cancel",
      "Unarchive and send",
      "Open the newer DM",
    ]);
  });

  it("ios with a newer DM keeps Open newer preferred first and Cancel last", () => {
    const buttons = build("ios", true);
    expect(buttons.map((b) => b.text)).toEqual([
      "Open the newer DM",
      "Unarchive and send",
      "Cancel",
    ]);
    expect(buttons[0]!.isPreferred).toBe(true);
    expect(buttons[2]!.style).toBe("cancel");
  });

  it("without a newer DM: Cancel then preferred Unarchive and send on both platforms", () => {
    for (const platform of ["ios", "android"]) {
      const buttons = build(platform, false);
      expect(buttons.map((b) => b.text)).toEqual(["Cancel", "Unarchive and send"]);
      expect(buttons[1]!.isPreferred).toBe(true);
    }
  });

  it("wires each button to its handler", () => {
    const onOpenNewer = vi.fn();
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    const buttons = buildArchivedSendAlertButtons({
      platform: "android", hasNewer: true, labels, onOpenNewer, onConfirm, onCancel,
    });
    for (const b of buttons) b.onPress();
    expect(onOpenNewer).toHaveBeenCalledTimes(1);
    expect(onConfirm).toHaveBeenCalledTimes(1);
    expect(onCancel).toHaveBeenCalledTimes(1);
  });
});

describe("unarchiveThenSendOnce (single send, draft retention)", () => {
  it("sends exactly once after a successful unarchive", async () => {
    const send = vi.fn().mockResolvedValue(undefined);
    await unarchiveThenSendOnce(() => Promise.resolve(true), send);
    expect(send).toHaveBeenCalledTimes(1);
  });

  it("does not send and rejects as intercepted when unarchive fails, so the draft is kept", async () => {
    const send = vi.fn();
    const err = await unarchiveThenSendOnce(() => Promise.resolve(false), send).catch(
      (e: unknown) => e,
    );
    expect(send).not.toHaveBeenCalled();
    expect(isArchivedSendIntercepted(err)).toBe(true);
  });

  it("propagates a send failure without a second send", async () => {
    const boom = new Error("send failed");
    const send = vi.fn().mockRejectedValue(boom);
    await expect(unarchiveThenSendOnce(() => Promise.resolve(true), send)).rejects.toBe(boom);
    expect(send).toHaveBeenCalledTimes(1);
  });
});

describe("patchSessionArchived (A2 cache patch)", () => {
  it("archiving unpins, marks archived and clears unread", () => {
    const s = makeSession({ id: "a", pinned: true, has_unread: true, unread_count: 3 });
    expect(patchSessionArchived(s, true)).toMatchObject({
      status: "archived",
      pinned: false,
      has_unread: false,
      unread_count: 0,
    });
  });

  it("unarchiving does not resurrect a pin and keeps unread state", () => {
    const s = makeSession({ id: "a", status: "archived", pinned: true, has_unread: true, unread_count: 2 });
    expect(patchSessionArchived(s, false)).toMatchObject({
      status: "active",
      pinned: false,
      has_unread: true,
      unread_count: 2,
    });
  });
});
