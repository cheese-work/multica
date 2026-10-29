import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import { forwardRef, type ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { TimelineEntry } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";

// CHE-2191: a comment body over 16,000 chars mounts a prefix of whole blocks
// (at most 2,000 chars, never splitting a block) and a "Show full comment"
// button; the full rich content mounts on click, when the comment is the
// highlighted target, or when find owns the thread.

vi.mock("@multica/core/api", () => ({
  api: { uploadFile: vi.fn() },
  dispatchReasonCode: () => undefined,
  errorCode: () => undefined,
}));

vi.mock("../../navigation", () => ({
  useNavigation: () => ({
    push: vi.fn(),
    pathname: "/acme/issues",
    getShareableUrl: (p: string) => `https://app.example${p}`,
  }),
}));

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: () => "Ada" }),
}));

vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: () => null,
}));

vi.mock("../hooks/use-comment-trigger-preview", () => ({
  useCommentTriggerPreview: () => ({ agents: [], blocked: [] }),
}));

vi.mock("../../editor", async () => ({
  ...(await vi.importActual<typeof import("../../editor/use-upload-gate")>("../../editor/use-upload-gate")),
  ...(await vi.importActual<typeof import("../../editor/use-lazy-editor")>("../../editor/use-lazy-editor")),
  ...(await vi.importActual<typeof import("../../editor/use-composer-submit")>("../../editor/use-composer-submit")),
  useEditorUpload: () => ({ uploadWithToast: vi.fn(), upload: vi.fn(), uploading: false }),
  useFileDropZone: () => ({ isDragOver: false, dropZoneProps: {} }),
  FileDropOverlay: () => null,
  // A blank-line-separated block starting with `|` stands in for a rich table.
  ReadonlyContent: ({ content }: { content: string }) => (
    <div data-testid="body">
      {content.split("\n\n").map((block, i) => block.startsWith("|")
        ? <table key={i}><tbody><tr><td>{block}</td></tr></tbody></table>
        : <p key={i}>{block}</p>)}
    </div>
  ),
  Attachment: () => null,
  AttachmentDownloadProvider: ({ children }: { children: ReactNode }) => <>{children}</>,
  ContentEditor: forwardRef(function MockContentEditor() {
    return <textarea data-testid="editor" />;
  }),
}));

import { CommentCard, type CommentCardProps } from "./comment-card";

const table = (n: number) => `| col ${n} | b |\n| --- | --- |\n| ${"x".repeat(2_000)} | y |`;
// Intro, then 3 tables each followed by prose: ~33 KB, tables all past the prefix.
const LONG = [
  "Plan rev-14",
  ...[1, 2, 3].flatMap((n) => [table(n), "prose ".repeat(1_600)]),
].join("\n\n");

function comment(id: string, content: string): TimelineEntry {
  return {
    type: "comment",
    id,
    actor_type: "member",
    actor_id: "user-1",
    content,
    parent_id: null,
    comment_type: "comment",
    reactions: [],
    attachments: [],
    created_at: "2026-09-11T07:00:00Z",
    updated_at: "2026-09-11T07:00:00Z",
    revision: 1,
  };
}

const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
const card = (content: string, extra: Partial<CommentCardProps> = {}) => (
  <QueryClientProvider client={qc}>
    <CommentCard
      issueId="issue-1"
      entry={comment("root", content)}
      replies={[]}
      currentUserId="user-1"
      onReply={vi.fn().mockResolvedValue(true)}
      onEdit={vi.fn().mockResolvedValue(undefined)}
      onDelete={vi.fn()}
      onToggleReaction={vi.fn()}
      {...extra}
    />
  </QueryClientProvider>
);
const renderCard = (content: string, extra: Partial<CommentCardProps> = {}) =>
  renderWithI18n(card(content, extra));

describe("CommentCard — oversized body (CHE-2191)", () => {
  it("fixture is over the limit", () => expect(LONG.length).toBeGreaterThan(30_000));

  it("mounts only a prefix and a Show full comment button", () => {
    renderCard(LONG);
    expect(screen.queryAllByRole("table")).toHaveLength(0);
    expect(screen.getByText("Plan rev-14")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Show full comment" })).toBeTruthy();
  });

  // The next block (a ~2,000-char table) does not fit in the 2,000-char budget,
  // so the prefix is the intro block alone: a block is never split.
  it("never splits a block: the prefix is whole blocks only", () => {
    renderCard(LONG);
    expect(screen.getByTestId("body").textContent).toBe("Plan rev-14");
  });

  it("fills the prefix with whole short blocks up to 2,000 chars", () => {
    const paragraphs = Array.from({ length: 30 }, (_, i) => `para ${i} `.padEnd(600, "x"));
    renderCard(paragraphs.join("\n\n"));
    const blocks = Array.from(screen.getByTestId("body").querySelectorAll("p"));
    expect(blocks.map((p) => p.textContent)).toEqual(paragraphs.slice(0, 3));
    expect(screen.getByRole("button", { name: "Show full comment" })).toBeTruthy();
  });

  // Fence boundaries follow Markdown, not a three-backtick count.
  it("never cuts inside a tilde-fenced block that holds blank lines", () => {
    const code = Array.from({ length: 200 }, (_, i) => `code line ${i}`).join("\n\n");
    renderCard(["intro", `~~~text\n${code}\n~~~`, "tail ".repeat(3_500)].join("\n\n"));
    expect(screen.getByTestId("body").textContent).toBe("intro");
  });

  it("keeps the closing fence of a four-backtick block holding a literal three-backtick line", () => {
    renderCard(["intro", "````\n```\n````", "tail ".repeat(3_500)].join("\n\n"));
    const text = screen.getByTestId("body").textContent ?? "";
    expect(text).toContain("````\n```\n````");
  });

  it("keeps an indented fence inside a list item whole", () => {
    const code = Array.from({ length: 200 }, (_, i) => `code line ${i}`).join("\n\n");
    renderCard(["intro", `- item\n\n  \`\`\`\n${code}\n  \`\`\``, "tail ".repeat(3_500)].join("\n\n"));
    expect(screen.getByTestId("body").textContent).not.toContain("code line");
  });

  it("mounts the full body on click and drops the button", () => {
    renderCard(LONG);
    fireEvent.click(screen.getByRole("button", { name: "Show full comment" }));
    expect(screen.getAllByRole("table")).toHaveLength(3);
    expect(screen.queryByRole("button", { name: "Show full comment" })).toBeNull();
  });

  it("renders full content for the highlighted target", () => {
    renderCard(LONG, { highlightedCommentId: "root" });
    expect(screen.getAllByRole("table")).toHaveLength(3);
    expect(screen.queryByRole("button", { name: "Show full comment" })).toBeNull();
  });

  it("renders full content while find owns the thread", () => {
    renderCard(LONG, { findManualCollapseDisabled: true });
    expect(screen.getAllByRole("table")).toHaveLength(3);
  });

  it("keeps the body open after the target highlight clears", () => {
    const { rerender } = renderCard(LONG, { highlightedCommentId: "root" });
    rerender(card(LONG, { highlightedCommentId: null }));
    expect(screen.getAllByRole("table")).toHaveLength(3);
  });

  it("leaves a body under the limit untouched", () => {
    renderCard(`short\n\n${table(1)}`);
    expect(screen.getAllByRole("table")).toHaveLength(1);
    expect(screen.queryByRole("button", { name: "Show full comment" })).toBeNull();
  });
});
