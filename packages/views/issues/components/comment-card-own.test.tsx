import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, within } from "@testing-library/react";
import { forwardRef, type ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { SupportedLocale } from "@multica/core/i18n";
import type { TimelineEntry } from "@multica/core/types";
import { useCommentCollapseStore } from "@multica/core/issues/stores";
import { renderWithI18n } from "../../test/i18n";

vi.mock("@multica/core/api", () => ({
  api: { uploadFile: vi.fn() },
  dispatchReasonCode: () => undefined,
  errorCode: () => undefined,
}));

vi.mock("../../navigation", () => ({
  useNavigation: () => ({
    push: vi.fn(),
    pathname: "/acme/issues",
    getShareableUrl: (path: string) => `https://app.example${path}`,
  }),
}));

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: (_type: string, id: string) => id }),
}));

vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => null }));
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
  ReadonlyContent: ({ content }: { content: string }) => <div>{content}</div>,
  Attachment: () => null,
  AttachmentDownloadProvider: ({ children }: { children: ReactNode }) => <>{children}</>,
  ContentEditor: forwardRef(function MockContentEditor() {
    return <textarea data-testid="editor" />;
  }),
}));

import { CommentCard } from "./comment-card";

function comment(id: string, extra: Partial<TimelineEntry> = {}): TimelineEntry {
  return {
    type: "comment",
    id,
    actor_type: "member",
    actor_id: "viewer",
    content: `body ${id}`,
    parent_id: null,
    comment_type: "comment",
    reactions: [],
    attachments: [],
    created_at: "2026-10-27T05:00:00Z",
    updated_at: "2026-10-27T05:00:00Z",
    revision: 1,
    ...extra,
  };
}

function thread(root: TimelineEntry, replies: TimelineEntry[] = [], options: {
  viewer?: string | null;
  highlightedId?: string;
  locale?: SupportedLocale;
} = {}) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <CommentCard
        issueId="own-comment-issue"
        entry={root}
        replies={replies}
        currentUserId={options.viewer === null ? undefined : options.viewer ?? "viewer"}
        highlightedCommentId={options.highlightedId}
        onReply={vi.fn().mockResolvedValue(true)}
        onEdit={vi.fn().mockResolvedValue(undefined)}
        onDelete={vi.fn()}
        onToggleReaction={vi.fn()}
      />
    </QueryClientProvider>,
    { locale: options.locale },
  );
}

function block(id: string) {
  return screen.getByText(`body ${id}`).closest("[data-comment-block]") as HTMLElement;
}

beforeEach(() => {
  useCommentCollapseStore.getState().expandAll("own-comment-issue");
});

describe("viewer-authored issue comments", () => {
  it.each(["root", "reply"])("fills only the viewer's member comment in the %s position", (position) => {
    const own = comment("own", { parent_id: position === "reply" ? "other" : null });
    const other = comment("other", { actor_id: "another-member", parent_id: position === "reply" ? null : "own" });
    const agent = comment("agent", { actor_type: "agent", actor_id: "viewer", parent_id: position === "reply" ? "other" : "own" });
    thread(position === "root" ? own : other, [position === "root" ? other : own, agent]);

    expect(block("own")).toHaveClass("comment-own");
    expect(within(block("own")).getByText("You")).toBeVisible();
    expect(block("own").querySelector(".sticky")).toHaveClass("comment-own");
    for (const id of ["other", "agent"]) {
      expect(block(id)).not.toHaveClass("comment-own");
      expect(within(block(id)).queryByText("You")).toBeNull();
    }
  });

  it("does not identify a comment as yours without a signed-in member", () => {
    thread(comment("root"), [], { viewer: null });
    expect(block("root")).not.toHaveClass("comment-own");
    expect(screen.queryByText("You")).toBeNull();
  });

  it.each(["root", "reply"])("lets a jump target override the own fill in the %s position", (position) => {
    const own = comment("own", { parent_id: position === "reply" ? "root" : null });
    thread(position === "root" ? own : comment("root", { actor_id: "other" }), position === "root" ? [] : [own], { highlightedId: "own" });
    expect(block("own")).not.toHaveClass("comment-own");
    expect(block("own")).toHaveClass("comment-target");
    expect(block("own").querySelector(".sticky")).toHaveClass("comment-target");
    expect(within(block("own")).getByText("You")).toBeVisible();
  });

  it("does not show authorship on a deleted root placeholder", () => {
    const { container } = thread(comment("root", { deleted_at: "2026-10-27T05:01:00Z", content: "" }), [comment("reply", { actor_id: "other", parent_id: "root" })]);
    expect(screen.getByText("This comment was deleted")).toBeVisible();
    expect(container.querySelector(".comment-own")).toBeNull();
    expect(screen.queryByText("You")).toBeNull();
  });

  it("keeps a resolved viewer reply identified without opening folded replies", () => {
    thread(comment("root", { actor_id: "other" }), [
      comment("folded", { parent_id: "root", actor_id: "other" }),
      comment("resolution", { parent_id: "root", resolved_at: "2026-10-27T05:02:00Z" }),
    ]);
    expect(screen.queryByText("body folded")).toBeNull();
    expect(block("resolution")).toHaveClass("comment-own");
    expect(within(block("resolution")).getByText("You")).toBeVisible();
  });

  it("keeps the own header when collapsed and while editing", async () => {
    const { container } = thread(comment("root"));
    fireEvent.click(screen.getByRole("button", { name: "Collapse thread" }));
    expect(container.querySelector("[data-comment-content]")).toBeNull();
    expect(screen.getByText("You").closest("[data-comment-block]")).toHaveClass("comment-own");
    fireEvent.click(screen.getByRole("button", { name: "Expand thread" }));
    fireEvent.click(screen.getByRole("button", { name: "Comment actions" }));
    fireEvent.click(await screen.findByText("Edit"));
    expect(await screen.findByTestId("editor")).toBeVisible();
    expect(container.querySelector("[data-comment-block]")).toHaveClass("comment-own");
    expect(screen.getByText("You")).toBeVisible();
  });

  it.each<[SupportedLocale, string]>([["en", "You"], ["zh-Hans", "你"], ["ko", "나"], ["ja", "あなた"], ["fr", "Vous"]])("labels the viewer in %s", (locale, label) => {
    thread(comment("root"), [], { locale });
    expect(within(block("root")).getByText(label)).toBeVisible();
  });
});
