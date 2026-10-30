import { describe, expect, it } from "vitest";
import {
  ChatPendingTaskSchema,
  ChatSessionSchema,
  ChatSessionUsageSchema,
  EMPTY_CHAT_SESSION_USAGE,
  SendChatMessageResponseSchema,
} from "./schemas";

describe("ChatPendingTaskSchema", () => {
  it("keeps the pending head and valid queue rows when one row is malformed", () => {
    const parsed = ChatPendingTaskSchema.parse({
      task_id: "task-active",
      status: "running",
      queued_tasks: [
        {
          task_id: "task-next",
          status: "queued",
          created_at: "2026-08-03T00:00:00Z",
        },
        { status: "queued" },
      ],
    });

    expect(parsed.task_id).toBe("task-active");
    expect(parsed.queued_tasks).toEqual([
      {
        task_id: "task-next",
        status: "queued",
        created_at: "2026-08-03T00:00:00Z",
      },
    ]);
  });
});

describe("SendChatMessageResponseSchema", () => {
  const base = {
    message_id: "message-1",
    task_id: "task-1",
    created_at: "2026-08-05T00:00:00Z",
  };

  it("keeps queue position parity with web", () => {
    expect(SendChatMessageResponseSchema.parse({ ...base, queued: false }).queued).toBe(false);
  });

  it("ignores a malformed additive queue position", () => {
    expect(SendChatMessageResponseSchema.parse({ ...base, queued: "no" }).queued).toBeUndefined();
  });
});

describe("ChatSessionSchema", () => {
  it("preserves pinned, lineage links, and last_message and defends against malformed additive fields", () => {
    const parsed = ChatSessionSchema.parse({
      id: "sess-1",
      workspace_id: "ws-1",
      creator_id: "user-1",
      agent_id: "agent-1",
      title: "Ops DM",
      status: "archived",
      pinned: "not-a-bool",
      continued_from_session_id: "sess-0",
      continued_into_session_id: 123,
      forked_from_session_id: "sess-root",
      last_message: {
        content: "Done",
        role: "unknown-role",
        created_at: "2026-10-21T10:00:00Z",
      },
      created_at: "2026-10-21T09:00:00Z",
      updated_at: "2026-10-21T10:00:00Z",
    });

    expect(parsed.status).toBe("archived");
    expect(parsed.pinned).toBeUndefined();
    expect(parsed.continued_from_session_id).toBe("sess-0");
    expect(parsed.continued_into_session_id).toBeNull();
    expect(parsed.forked_from_session_id).toBe("sess-root");
    expect(parsed.last_message?.role).toBe("assistant");
    expect(parsed.last_message?.created_at).toBe("2026-10-21T10:00:00Z");
  });
});

describe("ChatSessionUsageSchema", () => {
  it("parses session usage payload and exposes drift-sentinel EMPTY_CHAT_SESSION_USAGE", () => {
    const parsed = ChatSessionUsageSchema.parse({
      session_id: "sess-1",
      tokens: {
        input: 1000,
        output: 500,
        cache_read: 0,
        cache_write: 0,
      },
      uncosted_tokens: {
        input: 0,
        output: 0,
        cache_read: 0,
        cache_write: 0,
      },
      cost_usd_ticks: 120000000,
      runs: {
        by_status: { completed: 2, cancelled: 1, running: 1 },
        terminal: 3,
        metered: 2,
        unreported: 1,
        total_run_ms: 15000,
      },
      messages: {
        by_role: { user: 3, assistant: 2 },
      },
      by_model: {
        "claude-sonnet-4-5": {
          tokens: { input: 1000, output: 500, cache_read: 0, cache_write: 0 },
          cost_usd_ticks: 120000000,
        },
      },
      continued_from_session_id: "sess-0",
      continued_into_session_id: null,
      forked_from_session_id: null,
    });

    expect(parsed.session_id).toBe("sess-1");
    expect(parsed.runs.unreported).toBe(1);
    expect(Object.keys(parsed.by_model)).toEqual(["claude-sonnet-4-5"]);
    expect(EMPTY_CHAT_SESSION_USAGE.session_id).toBe("");
  });
});
