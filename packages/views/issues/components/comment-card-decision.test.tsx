import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import { forwardRef, type ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { TimelineEntry } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";

// replies arrives as a tombstone (`deleted_at` set, empty body). A tombstoned
// REPLY renders nothing — the replies it held already render in its place; a
// tombstoned ROOT keeps a placeholder, since it heads the thread. The cache
// rules live in packages/core/issues/comment-deletion.test.ts.

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
  useActorName: () => ({ getActorName: (_t: string, id: string) => ({ "human-1": "Cheese", "agent-1": "x99-claude-sonnet" } as Record<string, string>)[id] ?? id }),
}));

vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: ({ actorType, actorId }: { actorType: string; actorId: string }) => (
    <span data-testid="avatar" data-actor-type={actorType} data-actor-id={actorId} />
  ),
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
  ReadonlyContent: ({ content }: { content: string }) => <div>{content}</div>,
  Attachment: () => null,
  AttachmentDownloadProvider: ({ children }: { children: ReactNode }) => <>{children}</>,
  ContentEditor: forwardRef(function MockContentEditor() {
    return <textarea data-testid="editor" />;
  }),
}));

import { CommentCard } from "./comment-card";

// A decision quote is stored under the recording agent. The timeline shows the
// human whose words it holds, with the agent only as the card's sub-line.

vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/paths")>()),
  useWorkspaceSlug: () => "acme",
  useWorkspacePaths: () => ({ chatSession: (id: string) => `/acme/chat?session=${id}` }),
}));

vi.mock("@multica/core/issues/decision-disclaim", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/issues/decision-disclaim")>()),
  useDisclaimDecisionAnswer: () => ({ mutateAsync: vi.fn(), isPending: false }),
}));


function quote(extra: Partial<TimelineEntry> = {}): TimelineEntry {
  return {
    type: "comment",
    id: "q1",
    actor_type: "agent",
    actor_id: "agent-1",
    content: "> Yes, ship it.",
    parent_id: null,
    comment_type: "comment",
    reactions: [],
    attachments: [],
    created_at: "2026-10-23T05:00:00Z",
    updated_at: "2026-10-23T05:00:00Z",
    revision: 1,
    decision_comment_kind: "quote",
    decision_answer_id: "answer-1",
    decision_human_user_id: "human-1",
    decision_recorder_agent_id: "agent-1",
    decision_class: "reversible",
    decision_state: "effective",
    ...extra,
  };
}

function renderCard(entry: TimelineEntry, replies: TimelineEntry[] = []) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={qc}>
      <CommentCard
        issueId="issue-1"
        entry={entry}
        replies={replies}
        currentUserId="human-1"
        onReply={vi.fn().mockResolvedValue(true)}
        onEdit={vi.fn().mockResolvedValue(undefined)}
        onDelete={vi.fn()}
        onToggleReaction={vi.fn()}
      />
    </QueryClientProvider>,
  );
}

describe("CommentCard — decision quote", () => {
  it("keeps the comment card and the reply input the admit sheet names", () => {
    renderCard(quote());
    expect(screen.getByTestId("comment-card")).toBeTruthy();
    expect(screen.getByTestId("comment-reply-input")).toBeTruthy();
  });

  it("shows the human as the author and the agent only on the card", () => {
    renderCard(quote());
    const avatar = screen.getAllByTestId("avatar")[0]!;
    expect(avatar).toHaveAttribute("data-actor-type", "member");
    expect(avatar).toHaveAttribute("data-actor-id", "human-1");
    expect(screen.getByText("Cheese")).toBeTruthy();
    expect(screen.getByTestId("decision-quote-card")).toHaveTextContent("Recorded by x99-claude-sonnet");
    expect(screen.getByTestId("decision-quote-card")).toHaveTextContent("Yes, ship it.");
  });

  it("does not let the displayed human edit the agent-authored row", () => {
    renderCard(quote());
    fireEvent.click(screen.getAllByRole("button", { name: "Comment actions" })[0]!);
    expect(screen.queryByText("Edit")).toBeNull();
  });

  it("shows a quote reply the same way", () => {
    const root = quote({ id: "r0", decision_comment_kind: undefined, decision_answer_id: undefined, content: "Ask", actor_type: "agent" });
    renderCard(root, [quote({ id: "q2", parent_id: "r0" })]);
    expect(screen.getByTestId("decision-quote-card")).toBeTruthy();
    const avatars = screen.getAllByTestId("avatar").map((a) => a.getAttribute("data-actor-id"));
    expect(avatars).toContain("human-1");
  });

  it("puts the receipt under the member's own answering comment, not on a new one", () => {
    renderCard(
      quote({
        id: "m1",
        actor_type: "member",
        actor_id: "human-1",
        content: "Hold.",
        decision_comment_kind: undefined,
        decision_answer_id: undefined,
        decision_receipt_answer_id: "answer-1",
        decision_state: "pending",
        decision_chosen_option_index: 1,
      }),
    );
    expect(screen.getByText("Hold.")).toBeTruthy();
    const receipt = screen.getByTestId("decision-quote-card");
    expect(receipt).toHaveTextContent("Recorded");
    expect(receipt).toHaveTextContent("Option 2");
    expect(receipt).not.toHaveTextContent("Hold.");
    expect(screen.getByTestId("decision-disclaim-button")).toBeTruthy();
  });

  it("renders the withdrawal note as a compact line", () => {
    renderCard(quote({ id: "d1", decision_comment_kind: "disclaim", content: "Decision withdrawn: human." }));
    expect(screen.getByTestId("decision-withdrawal-line")).toHaveTextContent("Decision withdrawn: human.");
  });
});
