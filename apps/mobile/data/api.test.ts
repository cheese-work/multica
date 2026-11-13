import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { api } from "./api";
import { classifyIssueLoadError } from "@/lib/issue-load-error";
import type { SchemaValidationError } from "@/lib/parse-response";

// api.ts refuses to load without a base URL; set it before the import runs.
vi.hoisted(() => {
  process.env.EXPO_PUBLIC_API_URL = "https://api.example.test";
});

// The real store pulls in expo-secure-store; the client only needs the slug.
vi.mock("@/data/workspace-store", () => ({ getCurrentSlug: () => null }));

describe("api.deleteComment", () => {
  const fetchMock = vi.fn(async () => new Response(null, { status: 204 }));

  beforeEach(() => {
    fetchMock.mockClear();
    vi.stubGlobal("fetch", fetchMock);
    vi.spyOn(console, "log").mockImplementation(() => {});
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  // #8296: servers that keep a deleted comment's replies also route
  // /keep-replies; older servers do not, so a keep-replies delete that reaches
  // one fails instead of deleting the replies too.
  it.each([
    [{ keepReplies: true }, "https://api.example.test/api/comments/comment-1/keep-replies"],
    [{ keepReplies: false }, "https://api.example.test/api/comments/comment-1"],
    [undefined, "https://api.example.test/api/comments/comment-1"],
  ])("with %j sends DELETE %s", async (opts, url) => {
    await api.deleteComment("comment-1", opts);

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledWith(url, expect.objectContaining({ method: "DELETE" }));
  });
});

describe("api.createComment", () => {
  const fetchMock = vi.fn();

  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal("fetch", fetchMock);
    vi.spyOn(console, "log").mockImplementation(() => {});
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  // CHE-5286: the server folds an unmarked member comment into the agent's
  // running turn. Mobile shows no receipt for that, so it opts out.
  it("sends steer: false", async () => {
    fetchMock.mockResolvedValue(Response.json({}));

    await api.createComment("issue-1", "hello").catch(() => {});

    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("https://api.example.test/api/issues/issue-1/comments");
    expect(JSON.parse((init as RequestInit).body as string)).toMatchObject({ content: "hello", steer: false });
  });
});

describe("api.getCommentPreview", () => {
  const body = {
    comment_id: "c1",
    issue_id: "i1",
    issue_identifier: "CHE-1",
    author_type: "agent",
    author_id: "a1",
    author_name: "Luna",
    preview: "hello",
  };
  const fetchMock = vi.fn();

  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal("fetch", fetchMock);
    vi.spyOn(console, "log").mockImplementation(() => {});
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("GETs /api/comments/{id}/preview and returns the parsed body", async () => {
    fetchMock.mockResolvedValue(Response.json(body));

    await expect(api.getCommentPreview("c1")).resolves.toMatchObject(body);
    expect(fetchMock).toHaveBeenCalledWith(
      "https://api.example.test/api/comments/c1/preview",
      expect.anything(),
    );
  });

  it("returns null on 404 (unreadable or unknown comment)", async () => {
    fetchMock.mockResolvedValue(Response.json({ error: "not found" }, { status: 404 }));

    await expect(api.getCommentPreview("c1")).resolves.toBeNull();
  });

  it("returns null on a malformed body instead of a half-filled chip", async () => {
    fetchMock.mockResolvedValue(Response.json({ issue_id: "i1" }));

    await expect(api.getCommentPreview("c1")).resolves.toBeNull();
  });
});

describe("api chat DM stats, pin, and archive endpoints", () => {
  const fetchMock = vi.fn();

  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal("fetch", fetchMock);
    vi.spyOn(console, "log").mockImplementation(() => {});
    vi.spyOn(console, "warn").mockImplementation(() => {});
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("requests /api/chat/sessions?status=all when status is provided", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify([]), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );

    await api.listChatSessions({ status: "all" });

    expect(fetchMock).toHaveBeenCalledWith(
      "https://api.example.test/api/chat/sessions?status=all",
      expect.any(Object),
    );
  });

  it("fetches GET /api/chat/sessions/:id/usage and falls back to EMPTY_CHAT_SESSION_USAGE on schema drift", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ unexpected: true }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );

    const usage = await api.getChatSessionUsage("sess-1");

    expect(fetchMock).toHaveBeenCalledWith(
      "https://api.example.test/api/chat/sessions/sess-1/usage",
      expect.any(Object),
    );
    expect(usage.session_id).toBe("");
  });

  it("sends PATCH /api/chat/sessions/:id/archive with { archived }", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(
        JSON.stringify({
          id: "sess-1",
          workspace_id: "ws-1",
          creator_id: "u-1",
          agent_id: "a-1",
          title: "DM",
          status: "archived",
          created_at: "2026-10-21T00:00:00Z",
          updated_at: "2026-10-21T00:00:00Z",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );

    const session = await api.setChatSessionArchived("sess-1", true);

    expect(fetchMock).toHaveBeenCalledWith(
      "https://api.example.test/api/chat/sessions/sess-1/archive",
      expect.objectContaining({
        method: "PATCH",
        body: JSON.stringify({ archived: true }),
      }),
    );
    expect(session.status).toBe("archived");
  });

  it("sends PATCH /api/chat/sessions/:id/pin with { pinned }", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(
        JSON.stringify({
          id: "sess-1",
          workspace_id: "ws-1",
          creator_id: "u-1",
          agent_id: "a-1",
          title: "DM",
          status: "active",
          pinned: true,
          created_at: "2026-10-21T00:00:00Z",
          updated_at: "2026-10-21T00:00:00Z",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );

    const session = await api.setChatSessionPinned("sess-1", true);

    expect(fetchMock).toHaveBeenCalledWith(
      "https://api.example.test/api/chat/sessions/sess-1/pin",
      expect.objectContaining({
        method: "PATCH",
        body: JSON.stringify({ pinned: true }),
      }),
    );
    expect(session.pinned).toBe(true);
  });
});

describe("api chat supplements", () => {
  const fetchMock = vi.fn();
  const receipt = {
    message_id: "m1",
    chat_session_id: "s1",
    task_id: "t1",
    status: "pending",
    failure_reason: null,
    delivered_at: null,
    acked_after_resend_at: null,
    resent_task_id: null,
  };

  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal("fetch", fetchMock);
    vi.spyOn(console, "log").mockImplementation(() => {});
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("steers a queued follow-up with a POST to the steer route", async () => {
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify(receipt), { status: 200 }));

    const result = await api.steerChatTask("s1", "t1");

    expect(fetchMock).toHaveBeenCalledWith(
      "https://api.example.test/api/chat/sessions/s1/tasks/t1/steer",
      expect.objectContaining({ method: "POST" }),
    );
    expect(result.status).toBe("pending");
  });

  it.each([
    [undefined, false],
    [{ confirmPossibleDuplicate: true }, true],
  ])("resends with confirm_possible_duplicate only when asked (%j)", async (opts, expected) => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ task_id: "t2", supplement: receipt }), { status: 200 }),
    );

    await api.resendChatSupplement("s1", "m1", opts);

    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("https://api.example.test/api/chat/sessions/s1/messages/m1/supplement/resend");
    expect(JSON.parse((init as RequestInit).body as string)).toEqual({ confirm_possible_duplicate: expected });
  });
});

// CHE-3472: the decision park stores an object in metadata.decision_park. A
// strict primitive-only schema made mobile fall back to the empty issue, which
// the detail screen shows as "issue not found".
describe("api.getIssue", () => {
  const parked = {
    id: "01a1a0eb-718d-778e-9948-770bc5e04ad7", workspace_id: "ws", number: 3357,
    identifier: "CHE-3357", title: "Parked issue", description: "d",
    status: "need_decision", status_category: "in_progress", status_name: "Need Decision",
    priority: "medium", assignee_type: "agent", assignee_id: "a", creator_type: "agent", creator_id: "c",
    parent_issue_id: "p", project_id: "proj", position: -87, stage: 1, start_date: null, due_date: null,
    metadata: {
      decision_park: { readiness: "r1", readiness_id: "r2", set: "s", status: "blocked", to: "need_decision" },
      status_summary: "Blocked",
    },
    properties: {}, labels: [], created_at: "2026-11-03T10:19:07Z", updated_at: "2026-11-03T12:33:32Z",
  };

  it("returns the issue when metadata carries the decision-park object", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(parked), { status: 200 })));
    vi.spyOn(console, "log").mockImplementation(() => {});
    vi.spyOn(console, "warn").mockImplementation(() => {});

    const issue = await api.getIssue(parked.id);

    expect(issue.identifier).toBe("CHE-3357");
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });
});

describe("api.listWorkspacesChecked", () => {
  const fetchMock = vi.fn();
  const respond = (body: unknown) =>
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify(body), { status: 200 }));

  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal("fetch", fetchMock);
    vi.spyOn(console, "log").mockImplementation(() => {});
    vi.spyOn(console, "warn").mockImplementation(() => {});
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("validates a real list", async () => {
    respond([{ id: "ws1", slug: "a" }]);
    const result = await api.listWorkspacesChecked();
    expect(result.validated).toBe(true);
    expect(result.workspaces.map((w) => w.id)).toEqual(["ws1"]);
  });

  it("validates a real empty list", async () => {
    respond([]);
    expect(await api.listWorkspacesChecked()).toEqual({ workspaces: [], validated: true });
  });

  it.each([
    ["an object", { oops: 1 }],
    ["null", null],
    ["a list with a broken item", [{ id: "ws1" }, { slug: "no-id" }]],
  ])("falls back to an unvalidated empty list for %s", async (_name, body) => {
    respond(body);
    expect(await api.listWorkspacesChecked()).toEqual({ workspaces: [], validated: false });
  });
});

describe("api.getIssue schema failure", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("throws a typed error naming the failing path instead of returning an empty issue", async () => {
    vi.spyOn(console, "log").mockImplementation(() => {});
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(JSON.stringify({ id: "i1", number: "not-a-number" }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      ),
    );

    const err = await api.getIssue("i1").catch((e: unknown) => e);

    expect(classifyIssueLoadError(err)).toBe("data");
    expect((err as SchemaValidationError).paths).toContain("number");
    expect(String(warn.mock.calls[0]?.[0])).toContain("number");
  });
});
