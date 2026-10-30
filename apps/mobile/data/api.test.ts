import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { api } from "./api";

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
