import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, screen } from "@testing-library/react";
import { forwardRef, type ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { TimelineEntry } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";

// CHE-2272: inside an oversized comment, tables mount only near the viewport
// (a placeholder holds their space), because the CHE-2082 deep-link target
// alone mounts ~30 tables and ~65 SVG icons. Find still mounts them all.

vi.mock("@multica/core/api", () => ({
  api: { uploadFile: vi.fn(), getAttachmentTextContent: vi.fn() },
  PreviewTooLargeError: class extends Error {},
  PreviewUnsupportedError: class extends Error {},
  dispatchReasonCode: () => undefined,
  errorCode: () => undefined,
}));

vi.mock("../../navigation", () => ({
  useAppOrigin: () => "https://app.example",
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
  ...(await vi.importActual<typeof import("../../editor/readonly-content")>("../../editor/readonly-content")),
  Attachment: () => null,
  AttachmentDownloadProvider: ({ children }: { children: ReactNode }) => <>{children}</>,
  ContentEditor: forwardRef(function MockContentEditor() {
    return <textarea data-testid="editor" />;
  }),
}));

vi.mock("../hooks", () => ({ useResolveIssueIdentifier: () => null }));

import { CommentCard, type CommentCardProps } from "./comment-card";

const TABLES = 12;
const table = (n: number) => `| col ${n} | b |\n| --- | --- |\n| ${"x".repeat(200)} | y |`;
// ~16.5 KB of prose and 12 tables: over the 16,000-char limit.
const LONG = Array.from({ length: TABLES }, (_, i) => [table(i), "prose ".repeat(230)]).flat().join("\n\n");

type Observer = { callback: IntersectionObserverCallback; observed: Element[] };
let observers: Observer[] = [];

beforeEach(() => {
  observers = [];
  vi.stubGlobal("IntersectionObserver", class {
    private readonly self: Observer;
    constructor(callback: IntersectionObserverCallback) {
      this.self = { callback, observed: [] };
      observers.push(this.self);
    }
    observe(el: Element) { this.self.observed.push(el); }
    disconnect() { this.self.observed = []; }
    unobserve() {}
    takeRecords() { return []; }
  });
});
afterEach(() => vi.unstubAllGlobals());

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
const renderCard = (content: string, extra: Partial<CommentCardProps> = {}) =>
  renderWithI18n(
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
    </QueryClientProvider>,
  );

const placeholders = () => document.querySelectorAll("[data-rich-table-placeholder]");

describe("CommentCard — tables in an oversized body (CHE-2272)", () => {
  it("fixture is over the limit", () => expect(LONG.length).toBeGreaterThan(16_000));

  it("mounts no table of the deep-link target until it is near the viewport", () => {
    renderCard(LONG, { highlightedCommentId: "root" });
    expect(screen.queryAllByRole("table")).toHaveLength(0);
    expect(placeholders()).toHaveLength(TABLES);
  });

  it("mounts a table when its placeholder nears the viewport", () => {
    renderCard(LONG, { highlightedCommentId: "root" });
    const first = placeholders()[0]!;
    const observer = observers.find((o) => o.observed.includes(first))!;
    act(() => observer.callback([{ isIntersecting: true, target: first } as IntersectionObserverEntry], {} as IntersectionObserver));
    expect(screen.getAllByRole("table")).toHaveLength(1);
    expect(placeholders()).toHaveLength(TABLES - 1);
  });

  it("mounts every table while find owns the thread", () => {
    renderCard(LONG, { highlightedCommentId: "root", findManualCollapseDisabled: true });
    expect(screen.getAllByRole("table")).toHaveLength(TABLES);
    expect(placeholders()).toHaveLength(0);
  });

  it("leaves tables of a body under the limit mounted", () => {
    renderCard(`short\n\n${table(1)}`);
    expect(screen.getAllByRole("table")).toHaveLength(1);
    expect(placeholders()).toHaveLength(0);
  });
});
