// @vitest-environment node
import { webcrypto } from "node:crypto";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { MediaRoute } from "@multica/core/paths";
import { mediaContentId } from "./media-id";
import { markdownMedia, resolveMedia } from "./resolve-media";

const mocks = vi.hoisted(() => ({ getIssue: vi.fn(), listComments: vi.fn(), listAttachments: vi.fn(), getChatSession: vi.fn(), listChatMessages: vi.fn() }));
vi.mock("@multica/core/api", async (original) => ({ ...await original<object>(), api: mocks }));

const route: MediaRoute = { workspaceSlug: "acme", type: "issues", id: "issue-1", kind: "mermaid", mediaId: "placeholder" };
const chart = "graph TD\nA --> B";
const table = "| Name | Count |\n| --- | --- |\n| One | 1 |";
const html = "<h1>Only this media</h1>";

beforeEach(() => {
  vi.resetAllMocks();
  vi.stubGlobal("crypto", webcrypto);
  mocks.getIssue.mockResolvedValue({ id: "issue-1", workspace_id: "ws-1", description: `Private surrounding text\n\n\`\`\`mermaid\n${chart}\n\`\`\`\n\n${table}\n\n\`\`\`html\n${html}\n\`\`\``, attachments: [] });
  mocks.listComments.mockResolvedValue([]);
  mocks.listAttachments.mockResolvedValue([]);
  mocks.getChatSession.mockResolvedValue({ id: "chat-1", workspace_id: "ws-1" });
  mocks.listChatMessages.mockResolvedValue([{ content: `\`\`\`html\n${html}\n\`\`\`` }]);
});

describe("permission-checked media source resolution", () => {
  it.each([401, 403, 404])("does not resolve any media when issue read returns %s", async (status) => {
    mocks.getIssue.mockRejectedValue(Object.assign(new Error("Not found"), { status }));
    await expect(resolveMedia(route, "ws-1")).rejects.toMatchObject({ status });
    expect(mocks.listComments).not.toHaveBeenCalled();
    expect(mocks.listAttachments).not.toHaveBeenCalled();
    expect(mocks.listChatMessages).not.toHaveBeenCalled();
  });

  it("requires the source chat's read permission before loading messages", async () => {
    mocks.getChatSession.mockRejectedValue(Object.assign(new Error("Not found"), { status: 404 }));
    await expect(resolveMedia({ ...route, type: "chats", id: "chat-1" }, "ws-1")).rejects.toMatchObject({ status: 404 });
    expect(mocks.listChatMessages).not.toHaveBeenCalled();
  });

  it("does not resolve a source from a different workspace", async () => {
    await expect(resolveMedia(route, "ws-other")).rejects.toMatchObject({ status: 404 });
    expect(mocks.listComments).not.toHaveBeenCalled();
  });

  it.each([ ["mermaid", chart], ["table", table], ["html", html] ] as const)("returns only the chosen %s, including on a second load", async (kind, content) => {
    const chosen = { ...route, kind, mediaId: await mediaContentId(content) };
    expect(await resolveMedia(chosen, "ws-1")).toEqual({ kind, content });
    expect(await resolveMedia(chosen, "ws-1")).toEqual({ kind, content });
  });

  it("finds a comment's media but never follows a forged attachment ID", async () => {
    mocks.listComments.mockResolvedValue([{ content: `![Chart](https://example.test/chart.png)` }]);
    expect(await resolveMedia({ ...route, kind: "image", mediaId: await mediaContentId("https://example.test/chart.png") }, "ws-1")).toMatchObject({ kind: "image", source: { kind: "url", url: "https://example.test/chart.png" } });
    await expect(resolveMedia({ ...route, kind: "attachment", mediaId: "foreign-attachment" }, "ws-1")).rejects.toMatchObject({ status: 404 });
  });

  it("does not substitute a new item when the old content was edited", async () => {
    await expect(resolveMedia({ ...route, mediaId: await mediaContentId("graph TD\nC --> D") }, "ws-1")).rejects.toMatchObject({ status: 404 });
  });

  it("resolves an attached file only by its opaque identity within the allowed source", async () => {
    const attachment = { id: "attachment-private", url: "https://example.test/chart.png", filename: "chart.png", content_type: "image/png" };
    mocks.listAttachments.mockResolvedValue([attachment]);
    const chosen = { ...route, kind: "attachment" as const, mediaId: await mediaContentId(attachment.id) };
    expect(await resolveMedia(chosen, "ws-1")).toEqual({ kind: "attachment", source: { kind: "full", attachment } });
    await expect(resolveMedia({ ...chosen, mediaId: attachment.id }, "ws-1")).rejects.toMatchObject({ status: 404 });
  });

  it("resolves chat media only from the allowed session", async () => {
    expect(await resolveMedia({ ...route, type: "chats", id: "chat-1", kind: "html", mediaId: await mediaContentId(html) }, "ws-1")).toEqual({ kind: "html", content: html });
  });

  it("uses CommonMark fences and table extraction without upgrading an open fence", () => {
    expect(markdownMedia(`> ${table.replaceAll("\n", "\n> ")}\n\n\`\`\`mermaid\n${chart}`)).toEqual([{ kind: "table", content: table }]);
  });
});
