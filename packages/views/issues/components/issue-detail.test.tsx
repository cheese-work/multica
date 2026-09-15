import { forwardRef, useEffect, useRef, useState, useImperativeHandle, useSyncExternalStore } from "react";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Attachment, AgentTask, Issue, IssueStatusEntry, Label, TimelineEntry } from "@multica/core/types";
import { issueKeys } from "@multica/core/issues/queries";
import { issueStatusKeys } from "@multica/core/issue-statuses";
import { I18nProvider } from "@multica/core/i18n/react";
import { toast } from "sonner";
import { useResolvedExpandStore } from "@multica/core/issues/stores/resolved-expand-store";
// Imported from the barrel (not the submodule path) so this is the exact
// same module instance issue-detail.tsx reads through the barrel mock below
// — the submodule path resolves to a second, disconnected instance under
// Vitest's mock graph once the barrel is mocked.
import { useIssueDisclosureStore } from "@multica/core/issues/stores";
import {
  DEFAULT_SUB_ISSUE_ROW_PROPERTIES,
  useSubIssueDisplayStore,
} from "@multica/core/issues/stores/sub-issue-display-store";
import { ScrollRestorationProvider, type ScrollRestorationAdapter } from "../../platform";
import enCommon from "../../locales/en/common.json";
import enIssues from "../../locales/en/issues.json";

const TEST_RESOURCES = { en: { common: enCommon, issues: enIssues } };

const mockViewport = vi.hoisted(() => ({ isMobile: false }));

// Counts MockContentEditor mounts. This pins the description to exactly one
// eager editor per issue and catches stale editor reuse across issue switches.
const contentEditorMounts = vi.hoisted(() => ({ count: 0 }));
const descriptionSelectionAction = vi.hoisted(() => ({ current: undefined as { label: string; onSelect: () => void } | undefined }));
// Stable empty-attachments reference: the real store returns a shared constant
// so the `useCommentDraftStore(s => s.getAttachments(key))` selector keeps a
// stable identity. A fresh `[]` per call would loop useSyncExternalStore.
const emptyDraftAttachments = vi.hoisted(() => [] as unknown[]);

// Minimal real subscription backing for the useCommentDraftStore mock below,
// used only by the "latch survives reason ending" regression — every other
// test in this file needs `drafts: {}` and nothing more, so this stays
// separate from that fixed default rather than replacing it. Unlike
// `descriptionMeasurement.current` (set once before render, read on the next
// natural re-render), the latch test mutates `drafts` mid-test and needs
// React to actually re-render in response — a bare mutable ref does not do
// that on its own, so this backs the mock hook with a genuine
// useSyncExternalStore subscription, matching what real Zustand provides.
const mockDraftStoreState = vi.hoisted(() => {
  type Draft = { content: string; attachments: { status: string }[]; updatedAt: number };
  let drafts: Record<string, Draft> = {};
  const listeners = new Set<() => void>();
  return {
    getDrafts: () => drafts,
    setDraft: (key: string, draft: Draft | undefined) => {
      const next = { ...drafts };
      if (draft) next[key] = draft;
      else delete next[key];
      drafts = next;
      for (const listener of listeners) listener();
    },
    reset: () => {
      drafts = {};
      for (const listener of listeners) listener();
    },
    subscribe: (listener: () => void) => {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
  };
});

// Minimal real subscription backing for the useCommentCollapseStore mock
// below, used only by the collapsed-root/active-run regression — every other
// test needs "nothing manually collapsed" and nothing more, so this mirrors
// mockDraftStoreState's shape rather than replacing that fixed default.
// `getSnapshot` returns a fresh Set on every call (not the backing store
// object itself) so useSyncExternalStore's Object.is comparison actually
// detects a `setCollapsed` mutation — a snapshot getter that always returns
// the same object reference, with only its internal Set mutated in place,
// never signals React to re-render on its own.
const mockCollapseStoreState = vi.hoisted(() => {
  let collapsed = new Set<string>();
  const listeners = new Set<() => void>();
  return {
    isCollapsed: (id: string) => collapsed.has(id),
    getSnapshot: () => collapsed,
    setCollapsed: (id: string, value: boolean) => {
      const next = new Set(collapsed);
      if (value) next.add(id);
      else next.delete(id);
      collapsed = next;
      for (const listener of listeners) listener();
    },
    reset: () => {
      collapsed = new Set();
      for (const listener of listeners) listener();
    },
    subscribe: (listener: () => void) => {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
  };
});

// jsdom has no Range.getClientRects, and this file exercises description
// lifecycle/integration, not disclosure geometry (canonical coverage for the
// measurement math lives in description-measurement.test.ts and the
// disclosure primitive's own test in description-disclosure.test.tsx). Report
// no overflow by default so the description renders exactly as before these
// tests were written.
const descriptionMeasurement = vi.hoisted(() => ({
  current: {
    totalRows: 1,
    hiddenRows: 0,
    hasOverflow: false,
    lineHeight: 20,
    previewText: "",
  },
}));
vi.mock("./description-measurement", () => ({
  DESCRIPTION_PREVIEW_LINES: 12,
  measureDescription: () => descriptionMeasurement.current,
}));

// Controllable per-test: the description upload path's uploadWithToast.
const mockUploadWithToast = vi.hoisted(() => vi.fn());
// Controllable per-test: the description drop zone's captured onDrop, so a
// test can simulate a file drop without a real DataTransfer/DOM drop event.
const descDropZoneOnDrop = vi.hoisted(() => ({ current: null as ((files: File[]) => void) | null }));
// Spy on the description editor's imperative uploadFile so tests can assert
// call order against the disclosure store's expand call.
const descEditorUploadFile = vi.hoisted(() => vi.fn());
// Captured onUploadFile prop passed to the description's ContentEditor. The
// mocked editor's imperative uploadFile() is a bare spy (it has no document to
// insert a placeholder into), so tests that need the actual
// handleDescriptionUpload → uploadWithToast → bind path invoke this directly,
// exactly as the real editor would internally on a paste/drop/select.
const descOnUploadFile = vi.hoisted(() => ({ current: null as ((file: File) => Promise<unknown>) | null }));

vi.mock("@multica/ui/hooks/use-mobile", () => ({
  useIsMobile: () => mockViewport.isMobile,
}));

// useWorkspaceId() derives from useCurrentWorkspace (relative import inside
// @multica/core/hooks.tsx). vi.mock("@multica/core/paths") only intercepts
// the bare-specifier, not the internal relative import. Mock the hooks module
// directly so the bridge hook returns the test UUID.
vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "ws-1",
}));

// ---------------------------------------------------------------------------
// Mocks
// ---------------------------------------------------------------------------

// Mock @multica/core/auth
const mockAuthUser = { id: "user-1", email: "test@test.com", name: "Test User" };
vi.mock("@multica/core/auth", () => ({
  useAuthStore: Object.assign(
    (selector?: any) => {
      const state = { user: mockAuthUser, isAuthenticated: true };
      return selector ? selector(state) : state;
    },
    { getState: () => ({ user: mockAuthUser, isAuthenticated: true }) },
  ),
  registerAuthStore: vi.fn(),
  createAuthStore: vi.fn(),
}));

// Mock @multica/core/workspace/hooks
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({
    getMemberName: (id: string) => (id === "user-1" ? "Test User" : "Unknown"),
    getAgentName: (id: string) => (id === "agent-1" ? "Claude Agent" : "Unknown Agent"),
    getActorName: (type: string, id: string) => {
      if (type === "member" && id === "user-1") return "Test User";
      if (type === "agent" && id === "agent-1") return "Claude Agent";
      return "Unknown";
    },
    getActorInitials: (type: string) => (type === "member" ? "TU" : "CA"),
    getActorAvatarUrl: () => null,
  }),
}));

// Mock workspace queries
vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({
    queryKey: ["workspaces", "ws-1", "members"],
    queryFn: () => Promise.resolve([{ user_id: "user-1", name: "Test User", email: "test@test.com", role: "admin" }]),
  }),
  agentListOptions: () => ({
    queryKey: ["workspaces", "ws-1", "agents"],
    queryFn: () => Promise.resolve([]),
  }),
  squadListOptions: () => ({
    queryKey: ["workspaces", "ws-1", "squads"],
    queryFn: () => Promise.resolve([]),
  }),
  assigneeFrequencyOptions: () => ({
    queryKey: ["workspaces", "ws-1", "assignee-frequency"],
    queryFn: () => Promise.resolve([]),
  }),
  workspaceListOptions: () => ({
    queryKey: ["workspaces"],
    queryFn: () => Promise.resolve([{ id: "ws-1", name: "Test WS", slug: "test" }]),
  }),
}));

// Mock @multica/core/paths — after the URL-driven workspace refactor,
// useCurrentWorkspace / useWorkspacePaths derive from the workspace slug in
// URL Context. Tests don't mount a real route, so we short-circuit to fixtures.
vi.mock("@multica/core/paths", async () => {
  const actual = await vi.importActual<typeof import("@multica/core/paths")>(
    "@multica/core/paths",
  );
  return {
    ...actual,
    useCurrentWorkspace: () => ({ id: "ws-1", name: "Test WS", slug: "test" }),
    useWorkspacePaths: () => actual.paths.workspace("test"),
  };
});

// Mock navigation
vi.mock("../../navigation", () => ({
  AppLink: ({ children, href, ...props }: any) => (
    <a href={href} {...props}>
      {children}
    </a>
  ),
  useNavigation: () => ({
    push: vi.fn(),
    pathname: "/issues/issue-1",
    getShareableUrl: (p: string) => `https://app.multica.com${p}`,
  }),
  useBackOrReplace: () => vi.fn(),
  NavigationProvider: ({ children }: { children: React.ReactNode }) => children,
}));

// Mock editor components (Tiptap requires real DOM)
vi.mock("../../editor", async () => ({
  // Real lazy-mount controller (pure React, no Tiptap) so readonly-first
  // shell → activate → ready flows behave exactly as in production.
  ...(await vi.importActual<typeof import("../../editor/use-lazy-editor")>(
    "../../editor/use-lazy-editor",
  )),
  // Real submit gate (pure React) — see comment-composers.test.tsx.
  ...(await vi.importActual<typeof import("../../editor/use-upload-gate")>(
    "../../editor/use-upload-gate",
  )),
  // Real await-then-render submit hook (pure React) so comment/reply/edit
  // composers run the production submit path.
  ...(await vi.importActual<typeof import("../../editor/use-composer-submit")>(
    "../../editor/use-composer-submit",
  )),
  useEditorUpload: () => ({
    uploadWithToast: mockUploadWithToast,
    upload: vi.fn(),
    uploading: false,
  }),
  // issue-detail.tsx's description is the only unconditional useFileDropZone
  // caller that renders before any comment/reply composer in the tree (those
  // gate their own onDrop behind `enabled`, but a real per-instance ref is
  // still registered on every mount). Capture the first registration per
  // test/render pass so a later comment-card mount doesn't clobber it.
  useFileDropZone: ({ onDrop }: { onDrop: (files: File[]) => void }) => {
    if (!descDropZoneOnDrop.current) descDropZoneOnDrop.current = onDrop;
    return { isDragOver: false, dropZoneProps: {} };
  },
  FileDropOverlay: () => null,
  // No-op so comment-card's AttachmentList can render without hitting the
  // real API singleton; tests that care about download wiring should write
  // dedicated specs against `use-download-attachment.test.tsx`.
  useDownloadAttachment: () => vi.fn(),
  // Inert preview hook — comment-card's AttachmentList uses it to gate the
  // Eye button. Dedicated coverage lives in attachment-preview-modal.test.tsx.
  useAttachmentPreview: () => ({
    open: vi.fn(),
    tryOpen: () => false,
    modal: null,
  }),
  // Pass-through: the detail page wraps its column in the image-sequence
  // provider, but paging between images is covered in
  // image-sequence-context.test.tsx against the real provider.
  ImageSequenceProvider: ({ children }: { children: React.ReactNode }) =>
    children,
  isPreviewable: () => false,
  ReadonlyContent: ({ content }: { content: string }) => (
    <div data-testid="readonly-content">{content}</div>
  ),
  ContentEditor: forwardRef(function MockContentEditor(
    {
      defaultValue,
      value: syncedValue,
      onUpdate,
      placeholder,
      flushPendingOnUnmount,
      onReady,
      selectionAction,
      onUploadFile,
    }: any,
    ref: any,
  ) {
    const initialValue = syncedValue ?? defaultValue ?? "";
    if (syncedValue !== undefined) {
      descriptionSelectionAction.current = selectionAction;
      descOnUploadFile.current = onUploadFile ?? null;
    }
    const valueRef = useRef(initialValue);
    const baseRef = useRef(initialValue);
    const [editorValue, setEditorValue] = useState(initialValue);
    // Mirrors the real editor's dirty guard: once the user has typed, an
    // external `value` change is refused so it cannot clobber unsaved bytes.
    // Only the imperative adoptContent channel lands after that point.
    const dirtyRef = useRef(false);
    useEffect(() => {
      contentEditorMounts.count += 1;
      onReady?.();
      // eslint-disable-next-line react-hooks/exhaustive-deps
    }, []);
    useEffect(() => {
      if (syncedValue === undefined || dirtyRef.current) return;
      valueRef.current = syncedValue;
      baseRef.current = syncedValue;
      setEditorValue(syncedValue);
    }, [syncedValue]);
    useImperativeHandle(ref, () => ({
      getMarkdown: () => valueRef.current,
      clearContent: () => {
        valueRef.current = "";
        dirtyRef.current = false;
        setEditorValue("");
      },
      // The real handle applies content the `value` prop cannot land (a dirty
      // editor refuses external syncs) and does so without emitting an update.
      adoptContent: (markdown: string) => {
        valueRef.current = markdown;
        baseRef.current = markdown;
        dirtyRef.current = false;
        setEditorValue(markdown);
      },
      focus: () => {},
      focusAtCoords: () => {},
      // The top-level composer blurs after a posted comment (afterAccepted).
      blur: () => {},
      // Read by the submit-time upload gate; no uploads are exercised here.
      hasActiveUploads: () => false,
      // Placeholder rebuild contract: the real handle draws a card for an
      // upload the document is not showing and reports whether it landed.
      // Mocks track ids only — no document to draw into.
      insertUploadPlaceholder: () => true,
      settleUploadPlaceholder: () => false,
      uploadFile: descEditorUploadFile,
    }));
    return (
      <textarea
        value={editorValue}
        onChange={(e) => {
          valueRef.current = e.target.value;
          dirtyRef.current = true;
          setEditorValue(e.target.value);
          onUpdate?.(e.target.value, baseRef.current);
        }}
        placeholder={placeholder}
        data-testid="rich-text-editor"
        data-flush-on-unmount={flushPendingOnUnmount ? "true" : undefined}
      />
    );
  }),
  TitleEditor: forwardRef(function MockTitleEditor(
    { defaultValue, placeholder, onBlur, onChange, onReady }: any,
    ref: any,
  ) {
    const valueRef = useRef(defaultValue || "");
    const [value, setValue] = useState(defaultValue || "");
    useEffect(() => {
      onReady?.();
      // eslint-disable-next-line react-hooks/exhaustive-deps
    }, []);
    useImperativeHandle(ref, () => ({
      getText: () => valueRef.current,
      focus: () => {},
      focusAtCoords: () => {},
    }));
    return (
      <input
        value={value}
        onChange={(e) => {
          valueRef.current = e.target.value;
          setValue(e.target.value);
          onChange?.(e.target.value);
        }}
        onBlur={() => onBlur?.(valueRef.current)}
        placeholder={placeholder}
        data-testid="title-editor"
      />
    );
  }),
}));

// Mock common components
vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: ({ actorType, actorId }: any) => (
    <span data-testid="actor-avatar">
      {actorType}:{actorId}
    </span>
  ),
}));

vi.mock("../../projects/components/project-picker", () => ({
  ProjectPicker: () => <span data-testid="project-picker">Project</span>,
}));

// Mock api
const mockApiObj = vi.hoisted(() => ({
  getIssue: vi.fn(),
  listTimeline: vi.fn().mockResolvedValue([]),
  listComments: vi.fn().mockResolvedValue([]),
  createComment: vi.fn(),
  updateComment: vi.fn(),
  deleteComment: vi.fn(),
  deleteIssue: vi.fn(),
  updateIssue: vi.fn(),
  listIssueSubscribers: vi.fn().mockResolvedValue([]),
  subscribeToIssue: vi.fn().mockResolvedValue(undefined),
  unsubscribeFromIssue: vi.fn().mockResolvedValue(undefined),
  unsubscribeFromIssueSubtree: vi.fn().mockResolvedValue(undefined),
  getActiveTasksForIssue: vi.fn().mockResolvedValue({ tasks: [] }),
  listTasksByIssue: vi.fn().mockResolvedValue([]),
  rerunIssue: vi.fn(),
  listTaskMessages: vi.fn().mockResolvedValue([]),
  listChildIssues: vi.fn().mockResolvedValue({ issues: [] }),
  getChildIssueProgress: vi.fn().mockResolvedValue({ progress: [] }),
  getAgentTaskSnapshot: vi.fn().mockResolvedValue([]),
  // The sub-issues header chip reads this narrowed to the parent issue.
  getWorkspaceWorkingAgents: vi.fn().mockResolvedValue([]),
  listProperties: vi.fn().mockResolvedValue({ properties: [], total: 0 }),
  listIssues: vi.fn().mockResolvedValue({ issues: [], total: 0 }),
  uploadFile: vi.fn(),
  listIssueReactions: vi.fn().mockResolvedValue([]),
  addIssueReaction: vi.fn(),
  removeIssueReaction: vi.fn(),
  listAttachments: vi.fn().mockResolvedValue([]),
  addCommentReaction: vi.fn(),
  removeCommentReaction: vi.fn(),
  listMembers: vi.fn().mockResolvedValue([{ user_id: "user-1", name: "Test User", email: "test@test.com", role: "admin" }]),
  listAgents: vi.fn().mockResolvedValue([]),
  getAgent: vi.fn().mockResolvedValue(null),
  listRuntimes: vi.fn().mockResolvedValue([]),
  getProject: vi.fn(),
  listProjects: vi.fn().mockResolvedValue({ projects: [] }),
}));

vi.mock("@multica/core/api", () => ({
  api: mockApiObj,
  getApi: () => mockApiObj,
  setApiInstance: vi.fn(),
  errorCode: (error: unknown) =>
    typeof error === "object" && error !== null && "body" in error
      ? (error as { body?: { code?: string } }).body?.code
      : undefined,
}));

// Mock issue config
// Use the real status configuration so category fixtures cannot drift.

// Mock recent issues store
const mockRecordVisit = vi.fn();
vi.mock("@multica/core/issues/stores", async () => ({
  // Real store, not a stub: resolved-thread expand/collapse behavior under
  // test runs through it. Deep import keeps the persisted sibling stores
  // (which need localStorage) out of this mock.
  ...(await vi.importActual<
    typeof import("@multica/core/issues/stores/resolved-expand-store")
  >("@multica/core/issues/stores/resolved-expand-store")),
  // Real store: sub-issue display tests drive it with setState, and the
  // component reads it through the barrel — both must hit the same instance.
  ...(await vi.importActual<
    typeof import("@multica/core/issues/stores/sub-issue-display-store")
  >("@multica/core/issues/stores/sub-issue-display-store")),
  // Real store, in-memory (no localStorage): backs the sub-issues section's
  // collapsed state.
  ...(await vi.importActual<
    typeof import("@multica/core/issues/stores/sub-issues-collapse-store")
  >("@multica/core/issues/stores/sub-issues-collapse-store")),
  // Real store, non-persisted: backs description show-more/show-less across
  // this file's issue-switch and remount assertions.
  ...(await vi.importActual<
    typeof import("@multica/core/issues/stores/issue-disclosure-store")
  >("@multica/core/issues/stores/issue-disclosure-store")),
  useRecentIssuesStore: Object.assign(
    (selector?: any) => {
      const state = { byWorkspace: {}, recordVisit: mockRecordVisit, pruneWorkspaces: vi.fn() };
      return selector ? selector(state) : state;
    },
    {
      getState: () => ({
        byWorkspace: {},
        recordVisit: mockRecordVisit,
        pruneWorkspaces: vi.fn(),
      }),
    },
  ),
  selectRecentIssues: () => () => [],
  useCommentCollapseStore: (selector?: any) => {
    // Real subscription (not a bare re-invoked function): a mid-test
    // `setCollapsed` call actually schedules a re-render — every other
    // test's "nothing collapsed" default behaves identically to before,
    // since the backing store starts (and is reset) empty. The snapshot
    // getter must be `getSnapshot`, not a constant `() => mockCollapseStoreState`
    // — the latter returns the same object identity forever, so
    // useSyncExternalStore's Object.is check never observes a `setCollapsed`.
    useSyncExternalStore(mockCollapseStoreState.subscribe, mockCollapseStoreState.getSnapshot);
    const state = {
      collapsedByIssue: {},
      isCollapsed: (_issueId: string, commentId: string) => mockCollapseStoreState.isCollapsed(commentId),
      toggle: () => {},
    };
    return selector ? selector(state) : state;
  },
  useCommentDraftStore: Object.assign(
    (selector?: any) => {
      // Real subscription (not a bare re-invoked function): `drafts` is read
      // through useSyncExternalStore against `mockDraftStoreState`, so a
      // mid-test `setDraft`/`reset` call actually schedules a re-render —
      // every other test's `drafts: {}` default behaves identically to
      // before, since the backing store starts (and is reset) empty.
      const drafts = useSyncExternalStore(mockDraftStoreState.subscribe, mockDraftStoreState.getDrafts);
      const state = {
        drafts,
        getDraft: () => undefined,
        getAnnotations: () => emptyDraftAttachments,
        getAttachments: () => emptyDraftAttachments,
        getUploads: () => emptyDraftAttachments,
        setDraft: () => {},
        setAttachments: () => {},
        addUpload: () => {},
        settleUpload: () => {},
        failUpload: () => {},
        removeUpload: () => {},
        clearDraft: () => {},
      };
      return selector ? selector(state) : state;
    },
    {
      getState: () => ({
        drafts: mockDraftStoreState.getDrafts(),
        getDraft: () => undefined,
        getAnnotations: () => emptyDraftAttachments,
        getAttachments: () => emptyDraftAttachments,
        getUploads: () => emptyDraftAttachments,
        setDraft: () => {},
        setAttachments: () => {},
        addUpload: () => {},
        settleUpload: () => {},
        failUpload: () => {},
        removeUpload: () => {},
        clearDraft: () => {},
      }),
    },
  ),
  useCommentComposerStore: Object.assign(
    (selector?: any) => {
      const state = { sticky: true, toggleSticky: () => {} };
      return selector ? selector(state) : state;
    },
    {
      getState: () => ({ sticky: true, toggleSticky: () => {} }),
    },
  ),
}));

// Mock react-virtuoso: jsdom has no real layout, so the real Virtuoso would
// compute a 0-height viewport and render nothing. The mock renders every item
// inline so id="comment-..." nodes are always present in the DOM — this
// matches the production cold-path where `initialItemCount` force-mounts
// items[0..targetIdx], giving the deep-link effect a real target node.
//
// scrollIntoViewSpy: the deep-link effect no longer calls native
// scrollIntoView (it drives the timeline container's scrollTop directly to
// avoid scrolling ancestor overflow:hidden boxes — see issue-detail.tsx). We
// keep a no-op stub on the prototype so any stray scrollIntoView call from
// other components doesn't throw; deep-link tests assert the highlight
// background instead, which is mechanism-independent and observable without
// layout.
const scrollIntoViewSpy = vi.hoisted(() => vi.fn());
const scrollToIndexSpy = vi.hoisted(() => vi.fn());

vi.mock("react-virtuoso", () => ({
  Virtuoso: forwardRef(function MockVirtuoso(
    { data, itemContent }: { data: unknown[]; itemContent: (i: number, item: unknown) => unknown },
    ref: any,
  ) {
    useImperativeHandle(ref, () => ({
      // Real Virtuoso ref methods are not exercised by tests in this file
      // since the deep-link cold-path drives the container's scrollTop on the
      // real DOM node, not Virtuoso's imperative API.
      scrollIntoView: vi.fn(),
      scrollToIndex: scrollToIndexSpy,
    }));
    return (
      <div data-testid="virtuoso-mock">
        {data.map((item, i) => (
          <div key={i}>{itemContent(i, item) as React.ReactElement}</div>
        ))}
      </div>
    );
  }),
}));

// jsdom's HTMLElement.prototype.scrollIntoView is a no-op stub; replace it
// with a spy so the deep-link effect's call can be observed.
beforeEach(() => {
  scrollIntoViewSpy.mockClear();
  scrollToIndexSpy.mockClear();
  Object.defineProperty(HTMLElement.prototype, "scrollIntoView", {
    configurable: true,
    writable: true,
    value: scrollIntoViewSpy,
  });
  // The resolved-expand store is module-global (not per-mount like the old
  // useState); reset so one test's expansions can't leak into the next.
  useResolvedExpandStore.setState({ expandedByIssue: {} });
  // Same module-global concern for the description/thread disclosure store.
  useIssueDisclosureStore.setState({
    descriptionExpandedIssueIds: new Set(),
    expandedThreadIdsByIssue: {},
  });
  // Same concern for the useCommentDraftStore mock's backing state.
  mockDraftStoreState.reset();
  // Same concern for the useCommentCollapseStore mock's backing state.
  mockCollapseStoreState.reset();
});

// Mock modals
const mockOpenModal = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/modals", () => ({
  useModalStore: Object.assign(
    (selector?: (state: { open: typeof mockOpenModal }) => unknown) => {
      const state = { open: mockOpenModal };
      return selector ? selector(state) : state;
    },
    { getState: () => ({ open: mockOpenModal }) },
  ),
}));

// Mock core/hooks/use-file-upload
vi.mock("@multica/core/hooks/use-file-upload", () => ({
  useFileUpload: () => ({ uploadWithToast: vi.fn().mockResolvedValue("https://example.com/file.png") }),
}));

// Mock realtime
vi.mock("@multica/core/realtime", () => ({
  useWSEvent: vi.fn(),
  useWSReconnect: vi.fn(),
  useWS: () => ({ subscribe: vi.fn(() => () => {}), onReconnect: vi.fn(() => () => {}) }),
  WSProvider: ({ children }: { children: React.ReactNode }) => children,
  useRealtimeSync: () => {},
}));

// Mock sonner
vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn() },
}));

// Mock react-resizable-panels (used by @multica/ui/components/ui/resizable)
vi.mock("react-resizable-panels", () => ({
  Group: ({ children, ...props }: any) => <div data-testid="panel-group" {...props}>{children}</div>,
  Panel: ({ children, ...props }: any) => <div data-testid="panel" {...props}>{children}</div>,
  Separator: ({ children, ...props }: any) => <div data-testid="panel-handle" {...props}>{children}</div>,
  useDefaultLayout: () => ({ defaultLayout: undefined, onLayoutChanged: vi.fn() }),
  usePanelRef: () => ({ current: { isCollapsed: () => false, expand: vi.fn(), collapse: vi.fn() } }),
}));

// ---------------------------------------------------------------------------
// Test data
// ---------------------------------------------------------------------------

const mockIssue: Issue = {
  id: "issue-1",
  workspace_id: "ws-1",
  number: 1,
  identifier: "TES-1",
  title: "Implement authentication",
  description: "Add JWT auth to the backend",
  status: "in_progress",
  priority: "high",
  assignee_type: "member",
  assignee_id: "user-1",
  creator_type: "member",
  creator_id: "user-1",
  parent_issue_id: null,
  project_id: null,
  position: 0,
  stage: null,
  start_date: null,
  due_date: "2026-06-01T00:00:00Z",
  metadata: {},
  properties: {},
  created_at: "2026-01-15T00:00:00Z",
  updated_at: "2026-01-20T00:00:00Z",
  revision: 3,
};

const mockTimeline: TimelineEntry[] = [
  {
    type: "comment",
    id: "comment-1",
    actor_type: "member",
    actor_id: "user-1",
    content: "Started working on this",
    parent_id: null,
    created_at: "2026-01-16T00:00:00Z",
    updated_at: "2026-01-16T00:00:00Z",
    comment_type: "comment",
  },
  {
    type: "comment",
    id: "comment-2",
    actor_type: "agent",
    actor_id: "agent-1",
    content: "I can help with this",
    parent_id: null,
    created_at: "2026-01-17T00:00:00Z",
    updated_at: "2026-01-17T00:00:00Z",
    comment_type: "comment",
  },
];

// ---------------------------------------------------------------------------
// Import component under test (after mocks)
// ---------------------------------------------------------------------------

import { IssueDetail, groupSubIssuesByStage } from "./issue-detail";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function createTestQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: 0 },
      mutations: { retry: false },
    },
  });
}

function renderIssueDetail(issueId = "issue-1") {
  const queryClient = createTestQueryClient();
  return render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <QueryClientProvider client={queryClient}>
        <IssueDetail issueId={issueId} />
      </QueryClientProvider>
    </I18nProvider>,
  );
}

/**
 * The description's own Attach-file hidden input, disambiguated from every
 * other FileUploadButton on the page (comment composer, reply composer): it
 * is the one whose nearest ancestor containing the description editor is
 * closest (smallest depth) — every other input's nearest such ancestor is
 * the shared page root.
 */
function descriptionFileInput(): HTMLInputElement {
  const descriptionEditor = document.querySelector("[data-description-editor]");
  if (!descriptionEditor) throw new Error("description editor not found");
  const inputs = Array.from(document.querySelectorAll<HTMLInputElement>('input[type="file"]'));
  let closest: HTMLInputElement | null = null;
  let closestDepth = Number.POSITIVE_INFINITY;
  for (const input of inputs) {
    let depth = 0;
    for (let ancestor: HTMLElement | null = input.parentElement; ancestor; ancestor = ancestor.parentElement, depth++) {
      if (ancestor.contains(descriptionEditor)) {
        if (depth < closestDepth) {
          closest = input;
          closestDepth = depth;
        }
        break;
      }
    }
  }
  if (!closest) throw new Error("description Attach-file input not found");
  return closest;
}

/**
 * Renders with the workspace status catalog already in cache, so custom
 * statuses resolve to their real name, category and color. Seeding the query
 * (rather than stubbing the hook) keeps the shipped resolvers in the path; the
 * generous staleTime on the catalog query means it is never refetched. Every
 * other test runs without it — the cold-catalog case. (MUL-6243)
 */
function renderIssueDetailWithStatusCatalog(
  entries: IssueStatusEntry[],
  issueId = "issue-1",
) {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(issueStatusKeys.list("ws-1"), { statuses: entries });
  return render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <QueryClientProvider client={queryClient}>
        <IssueDetail issueId={issueId} />
      </QueryClientProvider>
    </I18nProvider>,
  );
}

function renderIssueDetailWithHighlight(
  highlightCommentId: string,
  issueId = "issue-1",
  options: { seedTimeline?: boolean } = {},
) {
  const queryClient = createTestQueryClient();
  if (options.seedTimeline) {
    // Pre-populate the timeline cache so the first render sees timeline.length>0.
    // This reproduces the inbox-click race: timeline data is available before
    // the issue itself has finished loading, so the effect that scrolls to
    // the comment fires once with `loading=true` (skeleton still rendered,
    // no comment DOM) and must re-fire when `loading` flips to false.
    queryClient.setQueryData(["issues", "timeline", issueId], mockTimeline);
  }
  const result = render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <QueryClientProvider client={queryClient}>
        <IssueDetail issueId={issueId} highlightCommentId={highlightCommentId} />
      </QueryClientProvider>
    </I18nProvider>,
  );
  return { ...result, queryClient };
}

const highlightedCommentBackgroundClass =
  "bg-[color-mix(in_srgb,var(--card)_95%,var(--brand)_5%)]";

function hasHighlightedCommentBackground(root: ParentNode | null): boolean {
  if (!root) return false;

  const elements = root instanceof Element
    ? [root, ...Array.from(root.querySelectorAll("[class]"))]
    : Array.from(root.querySelectorAll("[class]"));

  return elements.some(
    (el) => typeof el.className === "string" && el.className.includes(highlightedCommentBackgroundClass),
  );
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe("IssueDetail (shared)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    contentEditorMounts.count = 0;
    descriptionSelectionAction.current = undefined;
    descriptionMeasurement.current = {
      totalRows: 1,
      hiddenRows: 0,
      hasOverflow: false,
      lineHeight: 20,
      previewText: "",
    };
    mockUploadWithToast.mockReset();
    descDropZoneOnDrop.current = null;
    descEditorUploadFile.mockClear();
    descOnUploadFile.current = null;
    mockViewport.isMobile = false;
    // Default: issue loads successfully
    mockApiObj.getIssue.mockResolvedValue(mockIssue);
    // /timeline returns the entries flat in chronological order (oldest first).
    mockApiObj.listTimeline.mockResolvedValue(mockTimeline);
    mockApiObj.listIssueReactions.mockResolvedValue([]);
    mockApiObj.listIssueSubscribers.mockResolvedValue([]);
    mockApiObj.listChildIssues.mockResolvedValue({ issues: [] });
    mockApiObj.getChildIssueProgress.mockResolvedValue({ progress: [] });
    mockApiObj.getAgentTaskSnapshot.mockResolvedValue([]);
    mockApiObj.getWorkspaceWorkingAgents.mockResolvedValue([]);
    mockApiObj.listProperties.mockResolvedValue({ properties: [], total: 0 });
    mockApiObj.listIssues.mockResolvedValue({ issues: [], total: 0 });
    mockApiObj.getActiveTasksForIssue.mockResolvedValue({ tasks: [] });
    mockApiObj.listTasksByIssue.mockResolvedValue([]);
    mockApiObj.listTaskMessages.mockResolvedValue([]);
    mockApiObj.rerunIssue.mockResolvedValue({ id: "task-rerun" });
    mockApiObj.listMembers.mockResolvedValue([
      { user_id: "user-1", name: "Test User", email: "test@test.com", role: "admin" },
    ]);
    mockApiObj.listAgents.mockResolvedValue([]);
    // Reset project mock — individual tests override per case. Default fixture
    // has project_id: null so getProject is not invoked.
    mockApiObj.getProject.mockReset();
  });

  it("opens source-context creation from both a root comment and a reply", async () => {
    mockApiObj.listTimeline.mockResolvedValue([
      { ...mockTimeline[0], id: "source-root", parent_id: null },
      { ...mockTimeline[1], id: "source-reply", parent_id: "source-root" },
    ]);
    const { container } = renderIssueDetail();
    await screen.findByText("Started working on this");

    const menuButton = (commentId: string) => Array.from(
      container.querySelectorAll<HTMLButtonElement>('button[aria-label="Comment actions"]'),
    )
      .find((button): button is HTMLButtonElement => button?.closest("[id^='comment-']")?.id === `comment-${commentId}`);
    await waitFor(() => expect(menuButton("source-root")).toBeTruthy());
    await waitFor(() => expect(menuButton("source-reply")).toBeTruthy());

    fireEvent.click(menuButton("source-root")!);
    const branchAction = await screen.findByText("Create sub-issue from here");
    expect(branchAction.closest('[role="menuitem"]')?.querySelector(".lucide-message-square-plus")).toBeInTheDocument();
    fireEvent.click(branchAction);
    expect(mockOpenModal).toHaveBeenLastCalledWith("quick-create-issue", expect.objectContaining({
      anchor_comment_id: "source-root",
      parent_issue_id: "issue-1",
    }));

    fireEvent.click(menuButton("source-reply")!);
    fireEvent.click(await screen.findByText("Create sub-issue from here"));
    expect(mockOpenModal).toHaveBeenLastCalledWith("quick-create-issue", expect.objectContaining({
      anchor_comment_id: "source-reply",
      parent_issue_id: "issue-1",
    }));
  });

  it("does not offer source-context creation for system comments", async () => {
    mockApiObj.listTimeline.mockResolvedValue([{
      ...mockTimeline[0],
      id: "system-comment",
      comment_type: "system",
    }]);
    const { container } = renderIssueDetail();
    await screen.findByText("Started working on this");

    const menuButton = Array.from(
      container.querySelectorAll<HTMLButtonElement>('button[aria-label="Comment actions"]'),
    ).find((button) => button.closest("[id^='comment-']")?.id === "comment-system-comment");
    expect(menuButton).toBeTruthy();
    fireEvent.click(menuButton!);

    expect(screen.queryByText("Create sub-issue from here")).not.toBeInTheDocument();
  });

  it("shows loading skeleton while data is loading", () => {
    // Make the API hang to keep loading state
    mockApiObj.getIssue.mockReturnValue(new Promise(() => {}));
    renderIssueDetail();

    expect(
      screen.getAllByRole("generic").some((el) => el.getAttribute("data-slot") === "skeleton"),
    ).toBe(true);
  });

  it("gives the skeleton the same horizontal gutters as the loaded column", async () => {
    // The skeleton is the loaded column's stand-in, so a gutter change has to
    // land on both or the column jumps sideways at the moment the issue
    // arrives. Horizontal only: the loaded column also reserves the chat
    // launcher's corner at its bottom, which the skeleton has no scroll to
    // reach.
    const horizontalGutters = (el: Element | null) =>
      (el?.className ?? "")
        .split(/\s+/)
        .filter((cls) => /(^|:)px-/.test(cls))
        .sort();

    mockApiObj.getIssue.mockReturnValue(new Promise(() => {}));
    const loadingRender = renderIssueDetail();
    const skeletonGutters = horizontalGutters(
      loadingRender.container.querySelector(".max-w-4xl"),
    );
    loadingRender.unmount();

    mockApiObj.getIssue.mockResolvedValue(mockIssue);
    const { container } = renderIssueDetail();
    await waitFor(() => {
      expect(screen.getByText("Implement authentication")).toBeInTheDocument();
    });

    // Non-empty guard: without it a renamed column class passes vacuously.
    expect(skeletonGutters.length).toBeGreaterThan(0);
    expect(skeletonGutters).toEqual(
      horizontalGutters(container.querySelector(".max-w-4xl")),
    );
  });

  it("wires the description selection toolbar to annotation collection", async () => {
    renderIssueDetail();
    await screen.findByDisplayValue("Add JWT auth to the backend");
    expect(descriptionSelectionAction.current?.label).toBe("Add to comment");
    expect(descriptionSelectionAction.current?.onSelect).toBeTypeOf("function");
  });

  it("renders issue title and description after loading", async () => {
    renderIssueDetail();

    // The description is the one eager editor: keeping one renderer avoids the
    // layout jump caused by swapping a long react-markdown tree for ProseMirror.
    // Title and comment/reply composers remain readonly-first.
    expect(await screen.findByDisplayValue("Add JWT auth to the backend")).toBeInTheDocument();
    expect(screen.getByText("Implement authentication")).toBeInTheDocument();
    expect(screen.queryByTestId("title-editor")).not.toBeInTheDocument();
    expect(screen.getAllByTestId("rich-text-editor")).toHaveLength(1);
    expect(contentEditorMounts.count).toBe(1);
  });

  it("reconciles a cached list snapshot so source context appears on first entry", async () => {
    const sourceContext: NonNullable<Issue["source_context"]> = {
      id: "context-1",
      version: 1,
      usage: "read_only_historical_background",
      captured_at: "2026-08-21T12:00:00Z",
      display_state: "unchanged",
      source_issue_state: "unchanged",
      comment_thread_state: "unchanged",
      anchor_comment_state: "available",
      can_open_current_source: true,
      current_source: {
        issue_id: "source-issue",
        anchor_comment_id: "source-comment",
        identifier: "TES-7",
      },
      source_author_state: [],
      snapshot: {
        version: 1,
        captured_by_user_id: "user-1",
        captured_at: "2026-08-21T12:00:00Z",
        source_issue: {
          id: "source-issue",
          identifier: "TES-7",
          number: 7,
          title: "Source issue",
          description: "Source description",
          created_at: "2026-08-20T00:00:00Z",
          updated_at: "2026-08-21T00:00:00Z",
          revision: 2,
          attachments: [],
        },
        anchor_comment_id: "source-comment",
        comment_thread: [
          {
            id: "source-comment",
            parent_id: null,
            type: "comment",
            content: "Source comment",
            author: { type: "member", id: "user-1", name: "Test User" },
            created_at: "2026-08-21T01:00:00Z",
            updated_at: "2026-08-21T01:00:00Z",
            revision: 1,
            attachments: [],
          },
        ],
      },
    };
    const cachedWithoutDetailOnlyContext: Issue = {
      ...mockIssue,
      parent_issue_id: "source-issue",
    };
    const authoritativeDetail: Issue = {
      ...cachedWithoutDetailOnlyContext,
      source_context: sourceContext,
    };
    const parentIssue: Issue = {
      ...mockIssue,
      id: "source-issue",
      identifier: "TES-7",
      number: 7,
      title: "Source issue",
      parent_issue_id: null,
    };
    const queryClient = new QueryClient({
      defaultOptions: {
        queries: { retry: false, gcTime: 0, staleTime: Infinity },
        mutations: { retry: false },
      },
    });
    queryClient.setQueryData(
      ["issues", "ws-1", "detail", "issue-1"],
      cachedWithoutDetailOnlyContext,
    );
    mockApiObj.getIssue.mockImplementation((issueId: string) =>
      Promise.resolve(issueId === "source-issue" ? parentIssue : authoritativeDetail),
    );

    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={queryClient}>
          <IssueDetail issueId="issue-1" />
        </QueryClientProvider>
      </I18nProvider>,
    );

    const snapshotAction = await screen.findByRole("button", { name: "Context snapshot" });
    const summary = snapshotAction.closest<HTMLElement>('[data-slot="source-context-summary"]');
    expect(summary).toHaveTextContent("Sub-issue of");
    expect(within(summary!).getByRole("link", { name: "TES-7 Source issue" })).toBeInTheDocument();
    // The summary replaces the plain parent row rather than adding a second one.
    expect(screen.getAllByText("Sub-issue of")).toHaveLength(1);
    expect(screen.queryByText(/From TES-7/)).not.toBeInTheDocument();
    expect(mockApiObj.getIssue).toHaveBeenCalledWith("issue-1");
  });

  it("opts the description editor into the unmount flush", async () => {
    // Closing the issue modal must save the description the user last saw —
    // ContentEditor drops pending debounced updates on unmount by default
    // (so cancelled comment drafts aren't resurrected), and only this
    // explicit opt-in keeps a paste-then-close from losing the image
    // markdown and its attachment_ids bind (MUL-3254). The flush behavior
    // itself is covered in content-editor.test.tsx; this pins the wiring.
    renderIssueDetail();

    const description = await screen.findByDisplayValue("Add JWT auth to the backend");
    expect(description).toHaveAttribute("data-flush-on-unmount", "true");
  });

  it("remounts the eager description on issue switch without carrying stale content", async () => {
    // The web route reuses IssueDetail across issues. The keyed description
    // editor must remount atomically for the new issue while the title remains
    // on its cheap stand-in.
    const queryClient = createTestQueryClient();
    const issue2 = {
      ...mockIssue,
      id: "issue-2",
      title: "Second issue",
      description: "Second description",
    };
    // The cache seed marks the data stale, so the query refetches in the
    // background — getIssue must answer per-id or the refetch would clobber
    // issue-2 with issue-1's payload.
    mockApiObj.getIssue.mockImplementation((issueId: string) =>
      Promise.resolve(issueId === "issue-2" ? issue2 : mockIssue),
    );
    // Pre-seed issue-2 so its first render skips the loading skeleton.
    queryClient.setQueryData(["issues", "ws-1", "detail", "issue-2"], issue2);
    const ui = (issueId: string) => (
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={queryClient}>
          <IssueDetail issueId={issueId} />
        </QueryClientProvider>
      </I18nProvider>
    );
    const { rerender } = render(ui("issue-1"));

    await screen.findByDisplayValue("Add JWT auth to the backend");
    const mountsBeforeSwitch = contentEditorMounts.count;

    rerender(ui("issue-2"));

    expect(await screen.findByDisplayValue("Second description")).toBeInTheDocument();
    expect(screen.queryByDisplayValue("Add JWT auth to the backend")).not.toBeInTheDocument();
    expect(screen.queryByTestId("title-editor")).not.toBeInTheDocument();
    expect(contentEditorMounts.count).toBe(mountsBeforeSwitch + 1);
  });

  it("renders the issue title leaf as a link to the issue detail page", async () => {
    renderIssueDetail();

    // The breadcrumb leaf is the whole "identifier + title" string wrapped in a
    // single link to the issue's own detail route (used to open the full page
    // from the inline Inbox pane). A bare issue has no ancestor crumbs.
    const leaf = await screen.findByText("TES-1 Implement authentication");
    expect(leaf.closest("a")).toHaveAttribute("href", "/test/issues/issue-1");
  });

  it("omits the project breadcrumb segment when the issue has no project_id", async () => {
    // Default fixture has project_id: null.
    renderIssueDetail();

    // Leaf renders once loaded; a bare issue has no ancestor crumbs at all.
    await screen.findByText("TES-1 Implement authentication");

    // Project is never fetched and no project crumb appears.
    expect(mockApiObj.getProject).not.toHaveBeenCalled();
    expect(screen.queryByText("Marketing site refresh")).not.toBeInTheDocument();
  });

  it("renders the project breadcrumb segment when the issue belongs to a project", async () => {
    mockApiObj.getIssue.mockResolvedValue({ ...mockIssue, project_id: "p-1" });
    mockApiObj.getProject.mockResolvedValue({
      id: "p-1",
      workspace_id: "ws-1",
      title: "Marketing site refresh",
      description: null,
      icon: "🚀",
      status: "in_progress",
      priority: "none",
      lead_type: null,
      lead_id: null,
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
      issue_count: 0,
      done_count: 0,
      resource_count: 0,
    });

    renderIssueDetail();

    const projectLink = await screen.findByText("Marketing site refresh");
    // The whole project segment is a single AppLink pointing at the project
    // detail route under the active workspace slug.
    expect(projectLink.closest("a")).toHaveAttribute("href", "/test/projects/p-1");
  });

  it("renders properties sidebar with all core rows plus set optional rows", async () => {
    renderIssueDetail();

    await waitFor(() => {
      expect(screen.getByText("Properties")).toBeInTheDocument();
    });

    // Core rows — always rendered regardless of whether the issue has a value.
    expect(screen.getByText("Status")).toBeInTheDocument();
    expect(screen.getByText("Assignee")).toBeInTheDocument();
    // "Project" appears twice (row label + picker stub), so disambiguate by id.
    expect(screen.getByTestId("project-picker")).toBeInTheDocument();
    // priority="high" + due_date are set in the fixture, so both optional rows show.
    expect(screen.getByText("Priority")).toBeInTheDocument();
    expect(screen.getByText("Due date")).toBeInTheDocument();
    // No labels are attached in the fixture — the Labels optional row
    // must stay hidden by default.
    expect(screen.queryByText("Labels")).not.toBeInTheDocument();
    // Parent issue lives in its own section and only renders when the
    // issue actually has a parent — the fixture has none.
    expect(screen.queryByText("Parent issue")).not.toBeInTheDocument();
    // The "+ Add property" affordance is always offered while any
    // optional field is still hidden.
    expect(screen.getByText("Add property")).toBeInTheDocument();
  });

  it("hides every optional property row when none are set", async () => {
    // Override the default fixture: nothing optional set.
    mockApiObj.getIssue.mockResolvedValue({
      ...mockIssue,
      priority: "none",
      start_date: null,
      due_date: null,
    });

    renderIssueDetail();

    await waitFor(() => {
      expect(screen.getByText("Properties")).toBeInTheDocument();
    });

    expect(screen.queryByText("Priority")).not.toBeInTheDocument();
    expect(screen.queryByText("Due date")).not.toBeInTheDocument();
    expect(screen.queryByText("Labels")).not.toBeInTheDocument();
    // Project stays as a core row regardless of value.
    expect(screen.getByTestId("project-picker")).toBeInTheDocument();
    // No parent → no standalone Parent issue section either.
    expect(screen.queryByText("Parent issue")).not.toBeInTheDocument();
    expect(screen.getByText("Add property")).toBeInTheDocument();
  });

  it("uses a non-resizable layout with the sidebar sheet closed by default on mobile", async () => {
    mockViewport.isMobile = true;

    renderIssueDetail();

    await waitFor(() => {
      expect(screen.getByText("Implement authentication")).toBeInTheDocument();
    });

    expect(screen.queryByTestId("panel-group")).not.toBeInTheDocument();
    expect(screen.queryByText("Properties")).not.toBeInTheDocument();
  });

  it("pins the comment composer to the scroll viewport on a wide screen", async () => {
    const { container } = renderIssueDetail();

    await waitFor(() => {
      expect(screen.getByText("Implement authentication")).toBeInTheDocument();
    });

    // `bottom-0` is unique to the composer wrapper — the sticky affordances
    // inside the timeline (comment headers, resolve bars) all pin to `top-0`.
    expect(container.querySelector(".sticky.bottom-0")).not.toBeNull();
  });

  it("lets the composer ride the end of the timeline on mobile", async () => {
    // A pinned composer on a phone sits on the chat launcher's corner at every
    // scroll position, and the part it covers is its own send button.
    mockViewport.isMobile = true;

    const { container } = renderIssueDetail();

    await waitFor(() => {
      expect(screen.getByText("Implement authentication")).toBeInTheDocument();
    });

    expect(container.querySelector(".sticky.bottom-0")).toBeNull();
  });

  it("realigns after Virtuoso measures the newly posted row", async () => {
    mockApiObj.createComment.mockResolvedValue({
      id: "comment-new",
      issue_id: "issue-1",
      content: "A new update",
      author_type: "member",
      author_id: "user-1",
      parent_id: null,
      type: "comment",
      created_at: "2026-08-13T00:00:00Z",
      updated_at: "2026-08-13T00:00:00Z",
    });
    renderIssueDetail();

    await screen.findByText("Implement authentication");
    fireEvent.click(screen.getByTestId("comment-composer-shell"));
    const editor = await screen.findByPlaceholderText("Leave a comment...");
    fireEvent.change(editor, { target: { value: "A new update" } });
    const composer = editor.closest<HTMLElement>("[aria-busy], .relative.flex.flex-col.rounded-lg")!;
    fireEvent.click(within(composer).getByRole("button", { name: "Send" }));

    expect(scrollToIndexSpy).not.toHaveBeenCalled();
    await waitFor(() => {
      expect(scrollToIndexSpy).toHaveBeenCalledTimes(2);
    });
    expect(scrollToIndexSpy).toHaveBeenNthCalledWith(1, { index: 2, align: "end", offset: 0 });
    expect(scrollToIndexSpy).toHaveBeenNthCalledWith(2, { index: 2, align: "end", offset: 0 });
    expect(scrollIntoViewSpy).not.toHaveBeenCalled();
  });

  it("hides metadata content from the sidebar and shows a button when the bag has keys", async () => {
    // Metadata is agent-facing; the sidebar only exposes a button that opens
    // the raw JSON on demand. Keys are NOT rendered inline anywhere.
    mockApiObj.getIssue.mockResolvedValue({
      ...mockIssue,
      metadata: {
        pr_url: "https://example.com/pr/1",
        pipeline_status: "running",
      },
    });

    renderIssueDetail();

    await waitFor(() => {
      // Trigger label includes a "· N" count so users can see payload size
      // before clicking — accept any count via regex.
      expect(screen.getByRole("button", { name: /^Metadata\b/ })).toBeInTheDocument();
    });

    // Key names are not rendered in the sidebar prior to opening the dialog.
    expect(screen.queryByText("pr_url")).not.toBeInTheDocument();
    expect(screen.queryByText("pipeline_status")).not.toBeInTheDocument();
  });

  it("opens a dialog with formatted JSON when the Metadata button is clicked", async () => {
    mockApiObj.getIssue.mockResolvedValue({
      ...mockIssue,
      metadata: {
        pr_url: "https://example.com/pr/1",
        pipeline_status: "running",
      },
    });

    renderIssueDetail();

    const button = await screen.findByRole("button", { name: /^Metadata\b/ });
    fireEvent.click(button);

    // The dialog renders a <pre> containing the formatted JSON; checking the
    // exact serialized payload also verifies the indent / structure.
    const expected = JSON.stringify(
      { pr_url: "https://example.com/pr/1", pipeline_status: "running" },
      null,
      2,
    );
    await waitFor(() => {
      const pre = document.querySelector("pre");
      expect(pre).not.toBeNull();
      expect(pre!.textContent).toBe(expected);
    });
  });

  it("hides the Metadata button entirely when the bag is empty", async () => {
    // Default fixture already has metadata: {}, asserted explicitly here.
    renderIssueDetail();

    await waitFor(() => {
      expect(screen.getByText("Details")).toBeInTheDocument();
    });

    expect(screen.queryByRole("button", { name: /^Metadata\b/ })).not.toBeInTheDocument();
  });

  // Mapping edge cases live in comment-runs.test.ts; this verifies the shipped
  // IssueDetail -> CommentCard -> metadata card wiring as server caches change.
  it("keeps execution in an agent block before and after its persisted reply arrives", async () => {
    const taskId = "4a2e8d1c-7f9b-4e2a-9c1d-123456789abc";
    const task: AgentTask = {
      id: taskId, agent_id: "agent-1", runtime_id: "runtime-1", issue_id: "issue-1",
      status: "queued", priority: 0, created_at: "2026-01-16T00:00:00Z",
      started_at: null, dispatched_at: null, completed_at: null, result: null, error: null,
      trigger_comment_id: "comment-1", delivered_comment_ids: [],
    };
    const root = mockTimeline[0]!;
    mockApiObj.listTimeline.mockResolvedValue([]);
    mockApiObj.listTasksByIssue.mockResolvedValue([task]);
    mockApiObj.listTaskMessages.mockResolvedValue([
      { task_id: taskId, issue_id: "issue-1", seq: 1, type: "tool_use", tool: "exec_command", input: { command: "pnpm test" } },
    ]);
    const client = createTestQueryClient();
    const { container } = render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={client}>
          <IssueDetail issueId="issue-1" defaultSidebarOpen={false} />
        </QueryClientProvider>
      </I18nProvider>,
    );
    await waitFor(() => expect(client.getQueryData(issueKeys.tasks("issue-1"))).toEqual([task]));
    await waitFor(() => expect(client.getQueryData(issueKeys.timeline("issue-1"))).toEqual([]));
    expect(container.querySelector(`[data-run-id="${taskId}"]`)).toBeNull();
    mockApiObj.listTimeline.mockResolvedValue([root]);
    act(() => client.setQueryData(issueKeys.timeline("issue-1"), [root]));
    await screen.findByText("Waiting for an available agent.");
    const userBlock = container.querySelector(`#comment-body-${root.id}`)!.parentElement!;
    const agentBlock = container.querySelector(`[data-run-comment-id="${taskId}"]`)!;
    expect(agentBlock).not.toBeNull();
    expect(userBlock.contains(agentBlock)).toBe(false);
    expect(agentBlock.parentElement).toBe(userBlock.parentElement);
    expect(agentBlock.querySelector(`[data-run-id="${taskId}"]`)).not.toBeNull();
    const running: AgentTask = { ...task, status: "running", started_at: task.created_at, delivered_comment_ids: [root.id] };
    act(() => client.setQueryData(issueKeys.tasks("issue-1"), [running]));
    await screen.findByText("pnpm test");
    expect(container.querySelector(`[data-run-comment-id="${taskId}"]`)).toBe(agentBlock);
    expect(userBlock.querySelector("[data-run-id]")).toBeNull();

    fireEvent.click(within(agentBlock as HTMLElement).getByRole("button", { name: /View activity/ }));
    await within(agentBlock as HTMLElement).findByRole("button", { name: "Open full log" });

    const reply: TimelineEntry = {
      ...mockTimeline[1]!, id: "run-reply", parent_id: root.id, source_task_id: taskId,
      content: "Review complete. The navigation is ready.",
    };
    const completed: AgentTask = { ...running, status: "completed", completed_at: "2026-01-16T00:01:00Z" };
    mockApiObj.listTasksByIssue.mockResolvedValue([running]);
    mockApiObj.listTimeline.mockResolvedValue([root, reply]);
    act(() => {
      client.setQueryData(issueKeys.timeline("issue-1"), [root, reply]);
    });
    const body = await screen.findByText(reply.content!);
    const replyRow = container.querySelector("#comment-run-reply")!;
    await waitFor(() => expect(within(replyRow as HTMLElement).getByRole("button", { name: "Open full log" })).toBeInTheDocument());
    expect(within(replyRow as HTMLElement).queryByText("Completed")).not.toBeInTheDocument();
    const run = replyRow.querySelector(`[data-run-id="${taskId}"]`)!;
    expect(run.querySelector('[data-slot="card"]')).toBeNull();
    expect(container.querySelectorAll(`[data-run-id="${taskId}"]`)).toHaveLength(1);
    expect(run.contains(body)).toBe(false);
    expect(replyRow.contains(body)).toBe(true);
    expect(container.querySelector(`[data-run-comment-id="${taskId}"]`)).toBeNull();
    expect(userBlock.querySelector("[data-run-id]")).toBeNull();
    expect(within(replyRow as HTMLElement).queryByRole("button", { name: /View activity/ })).not.toBeInTheDocument();
    expect(within(run as HTMLElement).queryByRole("button", { name: "Stop" })).not.toBeInTheDocument();
    const logButton = within(run as HTMLElement).getByRole("button", { name: "Open full log" });
    expect(logButton.closest("[data-comment-block]")?.querySelector("[data-run-summary-row]")).toBeNull();
    mockApiObj.listTasksByIssue.mockResolvedValue([completed]);
    act(() => client.setQueryData(issueKeys.tasks("issue-1"), [completed]));
    await waitFor(() => expect(within(run as HTMLElement).queryByRole("button", { name: "Stop" })).not.toBeInTheDocument());
    expect(within(run as HTMLElement).getByRole("button", { name: "Open full log" })).toBe(logButton);
    fireEvent.click(within(run as HTMLElement).getByRole("button", { name: "Open full log" }));
    await screen.findByRole("dialog");
    expect(mockApiObj.listTaskMessages).toHaveBeenCalledWith(taskId);
  });

  it("places one coalesced queued block after the batch's latest reply", async () => {
    const root = mockTimeline[0]!;
    const first = { ...mockTimeline[1]!, id: "queued-first", parent_id: root.id,
      content: "First queued instruction", created_at: "2026-01-16T00:00:01Z" };
    const latest = { ...first, id: "queued-latest", content: "Latest queued instruction",
      created_at: "2026-01-16T00:00:02Z" };
    const task: AgentTask = {
      id: "4a2e8d1c-7f9b-4e2a-9c1d-123456789abd", agent_id: "agent-1", runtime_id: "runtime-1", issue_id: "issue-1",
      status: "queued", priority: 0, created_at: root.created_at,
      started_at: null, dispatched_at: null, completed_at: null, result: null, error: null,
      trigger_comment_id: latest.id, coalesced_comment_ids: [first.id], delivered_comment_ids: [],
    };
    mockApiObj.listTimeline.mockResolvedValue([root, first, latest]);
    mockApiObj.listTasksByIssue.mockResolvedValue([task]);
    const { container } = renderIssueDetail();

    await screen.findByText("Waiting for an available agent.");
    const run = container.querySelector(`[data-run-comment-id="${task.id}"]`)!;
    expect(run).not.toBeNull();
    const latestComment = container.querySelector(`#comment-${latest.id}`);
    const firstComment = container.querySelector(`#comment-${first.id}`);
    expect(latestComment).not.toBeNull();
    expect(firstComment).not.toBeNull();
    expect(latestComment!.nextElementSibling).toBe(run);
    expect(firstComment!.nextElementSibling).not.toBe(run);
  });

  it("keeps the running block after its delivered comment and one queued block after later replies", async () => {
    const root = mockTimeline[0]!;
    const first = { ...mockTimeline[1]!, id: "successor-first", parent_id: root.id,
      content: "First successor instruction", created_at: "2026-01-16T00:00:01Z" };
    const latest = { ...first, id: "successor-latest", content: "Latest successor instruction",
      created_at: "2026-01-16T00:00:02Z" };
    const running: AgentTask = {
      id: "4a2e8d1c-7f9b-4e2a-9c1d-123456789ab0", agent_id: "agent-1", runtime_id: "runtime-1", issue_id: "issue-1",
      status: "running", priority: 0, created_at: root.created_at,
      started_at: root.created_at, dispatched_at: root.created_at, completed_at: null, result: null, error: null,
      trigger_comment_id: root.id, delivered_comment_ids: [root.id],
    };
    const queued: AgentTask = {
      ...running,
      id: "4a2e8d1c-7f9b-4e2a-9c1d-123456789ab1",
      status: "queued", started_at: null, dispatched_at: null,
      trigger_comment_id: latest.id, coalesced_comment_ids: [first.id], delivered_comment_ids: [],
    };
    mockApiObj.listTimeline.mockResolvedValue([root, first, latest]);
    mockApiObj.listTasksByIssue.mockResolvedValue([running, queued]);
    const { container } = renderIssueDetail();

    await waitFor(() => {
      expect(container.querySelector(`[data-run-comment-id="${running.id}"]`)).not.toBeNull();
      expect(container.querySelector(`[data-run-comment-id="${queued.id}"]`)).not.toBeNull();
    });
    const rootContent = container.querySelector(`[data-comment-content="${root.id}"]`);
    const runningBlock = container.querySelector(`[data-run-comment-id="${running.id}"]`);
    const firstComment = container.querySelector(`#comment-${first.id}`);
    const latestComment = container.querySelector(`#comment-${latest.id}`);
    const queuedBlock = container.querySelector(`[data-run-comment-id="${queued.id}"]`);
    expect(rootContent).not.toBeNull();
    expect(runningBlock).not.toBeNull();
    expect(firstComment).not.toBeNull();
    expect(latestComment).not.toBeNull();
    expect(queuedBlock).not.toBeNull();
    expect(rootContent!.compareDocumentPosition(runningBlock!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(runningBlock!.compareDocumentPosition(firstComment!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(firstComment!.compareDocumentPosition(latestComment!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(latestComment!.nextElementSibling).toBe(queuedBlock);
    expect(container.querySelectorAll(`[data-run-comment-id="${queued.id}"]`)).toHaveLength(1);
  });

  it.each([null, "comment-1"])("shows an assignment reply once in its run slot when posted under %s", async (parentId) => {
    const task: AgentTask = {
      id: "ba2e8d1c-7f9b-4e2a-9c1d-123456789abc", agent_id: "agent-1", runtime_id: "runtime-1", issue_id: "issue-1",
      status: "queued", priority: 0, created_at: "2026-01-16T00:00:00Z",
      started_at: null, dispatched_at: null, completed_at: null, result: null, error: null,
      delivered_comment_ids: [],
    };
    const existing = { ...mockTimeline[0]!, id: "comment-1" };
    mockApiObj.listTimeline.mockResolvedValue([existing]);
    mockApiObj.listTasksByIssue.mockResolvedValue([task]);
    mockApiObj.listTaskMessages.mockResolvedValue([
      { task_id: task.id, issue_id: "issue-1", seq: 1, type: "tool_use", tool: "exec_command", input: { command: "pnpm test" } },
    ]);
    const queryClient = createTestQueryClient();
    const { container } = render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={queryClient}>
          <IssueDetail issueId="issue-1" defaultSidebarOpen={false} />
        </QueryClientProvider>
      </I18nProvider>,
    );
    await screen.findByText("Waiting for an available agent.");
    expect(container.querySelector(`[data-run-comment-id="${task.id}"]`)).not.toBeNull();
    const assignmentSlot = container.querySelector(`[data-run-slot-id="${task.id}"]`);
    const running: AgentTask = { ...task, status: "running", started_at: task.created_at };
    act(() => queryClient.setQueryData(issueKeys.tasks("issue-1"), [running]));
    await screen.findByText("pnpm test");
    expect(container.querySelectorAll(`[data-run-id="${task.id}"]`)).toHaveLength(1);
    const reply: TimelineEntry = {
      ...mockTimeline[1]!, id: "assignment-reply", parent_id: parentId, source_task_id: task.id,
      content: "Assignment complete.", created_at: "2026-01-16T00:01:00Z",
    };
    const completed: AgentTask = { ...running, status: "completed", completed_at: reply.created_at };
    mockApiObj.listTasksByIssue.mockResolvedValue([running]);
    mockApiObj.listTimeline.mockResolvedValue([existing, reply]);
    act(() => {
      queryClient.setQueryData(issueKeys.timeline("issue-1"), [existing, reply]);
    });
    await waitFor(() => expect(screen.getAllByText(reply.content!)).toHaveLength(1));
    await waitFor(() => expect(container.querySelector(`[data-run-comment-id="${task.id}"]`)).toBeNull());
    expect(container.querySelector(`[data-run-slot-id="${task.id}"]`)).toBe(assignmentSlot);
    const replyBlock = container.querySelector("#comment-assignment-reply")!;
    expect(replyBlock.querySelector(`[data-run-id="${task.id}"]`)).not.toBeNull();
    expect(container.querySelectorAll(`[data-run-id="${task.id}"]`)).toHaveLength(1);
    const headerLog = within(replyBlock as HTMLElement).getByRole("button", { name: "Open full log" });
    expect(replyBlock.querySelector("[data-run-summary-row]")).toBeNull();
    expect(within(replyBlock as HTMLElement).queryByRole("button", { name: "Stop" })).not.toBeInTheDocument();
    mockApiObj.listTasksByIssue.mockResolvedValue([completed]);
    act(() => queryClient.setQueryData(issueKeys.tasks("issue-1"), [completed]));
    await waitFor(() => expect(within(replyBlock as HTMLElement).queryByRole("button", { name: "Stop" })).not.toBeInTheDocument());
    expect(within(replyBlock as HTMLElement).getByRole("button", { name: "Open full log" })).toBe(headerLog);
  });

  // Canonical coverage for the latch itself (01-DESIGN "Durable state and
  // transition matrix", "New reply / edit / active run" row): an active
  // interaction must not just force a compact thread open for its duration —
  // it must LATCH the length-expanded preference so the thread stays open
  // after the interaction ends. `issue-disclosure-store.test.ts` only proves
  // `setThreadExpanded` is a correct setter; it cannot prove anything about
  // when the component calls it. This is that proof.
  it("latches a thread's length-expanded preference when an active reply draft forces it open, surviving after the draft clears", async () => {
    const root = mockTimeline[0]!;
    const replies: TimelineEntry[] = Array.from({ length: 4 }, (_, i) => ({
      ...mockTimeline[1]!,
      id: `latch-reply-${i}`,
      parent_id: root.id,
      content: `Reply ${i}`,
      created_at: `2026-01-16T00:0${i}:00Z`,
    }));
    mockApiObj.listTimeline.mockResolvedValue([root, ...replies]);
    renderIssueDetail();

    await screen.findByText("Reply 3");
    // Compact window: latest three visible (1, 2, 3), oldest (0) hidden.
    expect(screen.queryByText("Reply 0")).not.toBeInTheDocument();
    await screen.findByRole("button", { name: /Show \d+ more repl/ });

    // Introduce an active reply draft on the root — the same draft-store key
    // (`reply:${issueId}:${rootId}`) `rootIdsWithActiveReplyDraft` reads.
    act(() => {
      mockDraftStoreState.setDraft(`reply:issue-1:${root.id}`, {
        content: "typing a reply...",
        attachments: [],
        updatedAt: Date.now(),
      });
    });

    // forceThreadOpen's pin fires: the thread opens fully, including the
    // reply the compact window was hiding, and the latch effect persists
    // that expansion into useIssueDisclosureStore. Show less itself is
    // withheld while the pin is active (comment-card.tsx: `!forceThreadExpanded`
    // gates it) — that's a different, already-covered rule — so this only
    // asserts the reply is visible and the pin is what's showing it.
    await screen.findByText("Reply 0");
    expect(screen.queryByRole("button", { name: /Show \d+ more repl/ })).not.toBeInTheDocument();

    // Clear the draft — the interaction ends (reply sent), exactly like
    // useCommentDraftStore's clearDraft on a successful submit.
    act(() => {
      mockDraftStoreState.setDraft(`reply:issue-1:${root.id}`, undefined);
    });

    // The temporary pin is gone (no active draft), but 01-DESIGN line 56
    // requires the latch to survive: the previously-compact reply must
    // REMAIN visible, not refold now that the reason has cleared.
    await waitFor(() => expect(screen.queryByText("Reply 0")).toBeInTheDocument());
    expect(screen.getByRole("button", { name: "Show less" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Show \d+ more repl/ })).not.toBeInTheDocument();
    // Assert the durable store entry itself, not just rendered rows — the
    // rows alone can't distinguish "still latched" from "some other pin
    // happens to still be active."
    expect(useIssueDisclosureStore.getState().expandedThreadIdsByIssue["issue-1"]?.has(root.id)).toBe(true);
  });

  // CHE-479 regression: Fold All (search-command.tsx's foldAllCommentThreads
  // -> useIssueDisclosureStore.collapseAllThreads) must not have its
  // full-collapse intent immediately undone by the latch effect above for a
  // root whose only reason for compact-window absence is that Fold All
  // itself just cleared it. Before the fix, `collapseAllThreads` clearing
  // `expandedThreadIdsByIssue` changed the latch effect's
  // `expandedThreadLengths` dependency and re-fired it on the same tick;
  // with an active reply draft already present, `!expandedThreadLengths.has(rootId)`
  // read true again and the effect immediately re-latched the root open,
  // silently undoing the fold.
  it("does not let Fold All be immediately undone by the latch when a reply draft is already active on a folded root (CHE-479)", async () => {
    const root = mockTimeline[0]!;
    const replies: TimelineEntry[] = Array.from({ length: 4 }, (_, i) => ({
      ...mockTimeline[1]!,
      id: `foldall-latch-reply-${i}`,
      parent_id: root.id,
      content: `Foldall reply ${i}`,
      created_at: `2026-01-16T00:0${i}:00Z`,
    }));
    mockApiObj.listTimeline.mockResolvedValue([root, ...replies]);
    renderIssueDetail();

    await screen.findByText("Foldall reply 3");
    await screen.findByRole("button", { name: /Show \d+ more repl/ });

    // Start an active reply draft on the root — same latch reason as the
    // test above — so forceThreadOpen's pin is active and the latch effect
    // has already persisted the length-expansion for this root.
    act(() => {
      mockDraftStoreState.setDraft(`reply:issue-1:${root.id}`, {
        content: "typing a reply...",
        attachments: [],
        updatedAt: Date.now(),
      });
    });
    await screen.findByText("Foldall reply 0");
    expect(useIssueDisclosureStore.getState().expandedThreadIdsByIssue["issue-1"]?.has(root.id)).toBe(true);

    // Fold All: the same store call search-command.tsx's
    // foldAllCommentThreads makes on useIssueDisclosureStore.
    act(() => {
      useIssueDisclosureStore.getState().collapseAllThreads("issue-1");
    });

    // The fold must stick: the length-expanded entry stays cleared, even
    // though the reply draft is still active on this exact root. Continued
    // typing on this SAME still-active draft must not re-latch it either —
    // per 01-DESIGN line 56 the latch fires on a reason *starting*, not on
    // every keystroke of a reason that is already accounted for, and Fold
    // All's own row explicitly only promises the temporary pin ("Active
    // edit pins prevent focus loss") keeps the draft reachable, not that the
    // length preference stays expanded.
    expect(useIssueDisclosureStore.getState().expandedThreadIdsByIssue["issue-1"]?.has(root.id)).toBeFalsy();
    act(() => {
      mockDraftStoreState.setDraft(`reply:issue-1:${root.id}`, {
        content: "typing a reply... continued",
        attachments: [],
        updatedAt: Date.now(),
      });
    });
    await screen.findByText("Foldall reply 0");
    expect(useIssueDisclosureStore.getState().expandedThreadIdsByIssue["issue-1"]?.has(root.id)).toBeFalsy();

    // A genuinely NEW reason after the fold — this draft ending and a fresh
    // one starting on the same root — must still latch normally: Fold All's
    // suppression must not permanently block future latching.
    act(() => {
      mockDraftStoreState.setDraft(`reply:issue-1:${root.id}`, { content: "", attachments: [], updatedAt: Date.now() });
    });
    await waitFor(() => expect(useIssueDisclosureStore.getState().expandedThreadIdsByIssue["issue-1"]?.has(root.id)).toBeFalsy());
    act(() => {
      mockDraftStoreState.setDraft(`reply:issue-1:${root.id}`, {
        content: "a brand new reply draft",
        attachments: [],
        updatedAt: Date.now(),
      });
    });
    await waitFor(() =>
      expect(useIssueDisclosureStore.getState().expandedThreadIdsByIssue["issue-1"]?.has(root.id)).toBe(true),
    );
  });

  // CHE-479: a second Fold All must not resurrect suppression for a root
  // that the FIRST Fold All folded but that has since organically re-latched
  // (its draft ended and a new one started, per the test above), if that
  // second Fold All did not itself re-fold this root. `justFoldedRootIdsByIssue`
  // is overwritten wholesale by each `collapseAllThreads` call — this proves
  // a stale suppression entry from an earlier fold occurrence cannot leak
  // into a later, unrelated one.
  it("does not let a stale fold suppression from an earlier Fold All block a later, unrelated latch (CHE-479)", async () => {
    const root = mockTimeline[0]!;
    const other = { ...mockTimeline[1]!, id: "foldall-stale-other-root", content: "Other root" };
    const replies: TimelineEntry[] = Array.from({ length: 4 }, (_, i) => ({
      ...mockTimeline[1]!,
      id: `foldall-stale-reply-${i}`,
      parent_id: root.id,
      content: `Stale fold reply ${i}`,
      created_at: `2026-01-16T00:0${i}:00Z`,
    }));
    mockApiObj.listTimeline.mockResolvedValue([root, other, ...replies]);
    renderIssueDetail();
    await screen.findByRole("button", { name: /Show \d+ more repl/ });

    // Expand root, then fold everything (first Fold All folds `root`).
    act(() => {
      useIssueDisclosureStore.getState().expandAllThreads("issue-1", [root.id]);
    });
    act(() => {
      useIssueDisclosureStore.getState().collapseAllThreads("issue-1");
    });
    expect(useIssueDisclosureStore.getState().justFoldedRootIdsByIssue["issue-1"]?.has(root.id)).toBe(true);

    // A second Fold All happens with nothing expanded (e.g. the user ran the
    // command again with everything already compact) — it no-ops and does
    // NOT produce a new justFoldedRootIdsByIssue entry naming `root`, so the
    // original suppression should no longer apply to `root` once superseded
    // by any later, distinct justFoldedRoots state.
    act(() => {
      useIssueDisclosureStore.getState().expandAllThreads("issue-1", [other.id]);
    });
    act(() => {
      useIssueDisclosureStore.getState().collapseAllThreads("issue-1");
    });
    expect(useIssueDisclosureStore.getState().justFoldedRootIdsByIssue["issue-1"]?.has(root.id)).toBeFalsy();

    // Now start a fresh reply draft on `root` — this must latch normally:
    // the second, unrelated Fold All's justFoldedRoots (naming only `other`)
    // must not carry forward suppression for `root`.
    act(() => {
      mockDraftStoreState.setDraft(`reply:issue-1:${root.id}`, {
        content: "a fresh reply after an unrelated fold",
        attachments: [],
        updatedAt: Date.now(),
      });
    });
    await waitFor(() =>
      expect(useIssueDisclosureStore.getState().expandedThreadIdsByIssue["issue-1"]?.has(root.id)).toBe(true),
    );
  });

  it("latches a thread's length-expanded preference when an active run forces it open, surviving after the run completes", async () => {
    const root = mockTimeline[0]!;
    const replies: TimelineEntry[] = Array.from({ length: 4 }, (_, i) => ({
      ...mockTimeline[1]!,
      id: `latch-run-reply-${i}`,
      parent_id: root.id,
      content: `Run reply ${i}`,
      created_at: `2026-01-16T00:0${i}:00Z`,
    }));
    // An active (queued) run anchored to the newest reply, with no published
    // reply of its own yet — the exact shape `rootIdsWithActiveRun` looks for.
    const task: AgentTask = {
      id: "ba2e8d1c-7f9b-4e2a-9c1d-latchrun001", agent_id: "agent-1", runtime_id: "runtime-1", issue_id: "issue-1",
      status: "queued", priority: 0, created_at: "2026-01-16T00:05:00Z",
      started_at: null, dispatched_at: null, completed_at: null, result: null, error: null,
      trigger_comment_id: replies[3]!.id, delivered_comment_ids: [],
    };
    mockApiObj.listTimeline.mockResolvedValue([root, ...replies]);
    mockApiObj.listTasksByIssue.mockResolvedValue([task]);
    const client = createTestQueryClient();
    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={client}><IssueDetail issueId="issue-1" /></QueryClientProvider>
      </I18nProvider>,
    );

    // The active run is present from the first render (it's in the initial
    // `listTasksByIssue` mock, unlike the draft scenario above where the
    // draft starts mid-test), so the pin is already active by the time
    // anything mounts — the compact-window reply is visible immediately, and
    // the latch effect has already persisted the expansion.
    await screen.findByText("Run reply 3");
    await screen.findByText("Run reply 0");
    expect(useIssueDisclosureStore.getState().expandedThreadIdsByIssue["issue-1"]?.has(root.id)).toBe(true);

    // The run completes — the pin's reason ends.
    const completed: AgentTask = { ...task, status: "completed", completed_at: "2026-01-16T00:06:00Z" };
    mockApiObj.listTasksByIssue.mockResolvedValue([completed]);
    act(() => {
      client.setQueryData(issueKeys.tasks("issue-1"), [completed]);
    });

    // 01-DESIGN line 56 requires the latch to survive run completion exactly
    // as it does draft-clearing: the reply stays visible and the store entry
    // is not removed.
    await waitFor(() => expect(screen.queryByText("Run reply 0")).toBeInTheDocument());
    expect(useIssueDisclosureStore.getState().expandedThreadIdsByIssue["issue-1"]?.has(root.id)).toBe(true);
  });

  // Regression for the gap Sol's review caught in PR #31 (CHE-380 stage 6):
  // `rootIdsWithActiveRun` originally only matched a run's `anchorCommentId`
  // against the thread's REPLIES, so a run anchored directly to the root
  // comment itself (no reply on it yet) never entered the set. Because
  // root-anchored runs render inside comment-card.tsx's own `open` gate
  // (`!isCollapsed || forceThreadExpanded`), a manually collapsed root could
  // hide a newly active run with no way to reveal it short of manually
  // un-collapsing. `anchorCommentId === rootId` closes that gap.
  //
  // Sol's follow-up review required the lifecycle to be observed, not
  // assumed: render collapsed with no run present, THEN inject the
  // root-anchored run so its slot's appearance is an actual assertion
  // rather than baked into the initial mock data, and check the manual
  // collapse preference itself (not just the Stop button) both while the
  // run is active and after it completes.
  it("force-opens a manually collapsed root when an active run anchors directly to the root, not just to one of its replies", async () => {
    const root = mockTimeline[0]!;
    const taskId = "ba2e8d1c-7f9b-4e2a-9c1d-rootanchor01";
    // Root is manually collapsed with no run in play yet — the starting
    // state Sol's review requires observing before any run is injected.
    mockCollapseStoreState.setCollapsed(root.id, true);
    mockApiObj.listTimeline.mockResolvedValue([root]);
    mockApiObj.listTasksByIssue.mockResolvedValue([]);
    const client = createTestQueryClient();
    const { container } = render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={client}><IssueDetail issueId="issue-1" /></QueryClientProvider>
      </I18nProvider>,
    );
    await waitFor(() => expect(client.getQueryData(issueKeys.tasks("issue-1"))).toEqual([]));
    expect(container.querySelector(`[data-run-id="${taskId}"]`)).toBeNull();
    expect(mockCollapseStoreState.isCollapsed(root.id)).toBe(true);

    // Inject the root-anchored queued run after the collapsed/no-run state
    // has already rendered, mirroring how a run actually arrives mid-session.
    const task: AgentTask = {
      id: taskId, agent_id: "agent-1", runtime_id: "runtime-1", issue_id: "issue-1",
      status: "queued", priority: 0, created_at: "2026-01-16T00:05:00Z",
      started_at: null, dispatched_at: null, completed_at: null, result: null, error: null,
      trigger_comment_id: root.id, delivered_comment_ids: [],
    };
    mockApiObj.listTasksByIssue.mockResolvedValue([task]);
    act(() => {
      client.setQueryData(issueKeys.tasks("issue-1"), [task]);
    });

    // `forceThreadOpen`'s pin must beat manual collapse: the root-anchored
    // run's slot has to become visible despite `isCollapsed` being true, or
    // the active output is invisible with no affordance to reveal it. Check
    // both the DOM slot and the underlying collapse preference itself — the
    // pin must override the gate without mutating the preference it overrides.
    await screen.findByRole("button", { name: "Stop" });
    expect(container.querySelector(`[data-run-id="${taskId}"]`)).not.toBeNull();
    expect(mockCollapseStoreState.isCollapsed(root.id)).toBe(true);

    // The run completes — `isActiveCommentRun` no longer matches it, so
    // `rootIdsWithActiveRun` drops the root and `forceThreadOpen`'s pin
    // releases. Manual collapse was never mutated by the pin, so with
    // `isCollapsed` still true the root's `open` gate closes again and the
    // root-anchored run's slot — including its "Completed" summary, which
    // only rendered because the pin forced `open` true — leaves the DOM.
    // `delivered_comment_ids` is set to the root here because that is what a
    // real completion does (comment-runs.ts's anchor resolution reads
    // `delivered_comment_ids` once a task is no longer queued/dispatched,
    // per buildCommentRunView's `usesPlannedCoverage`); leaving it `[]` (as
    // it correctly is at `queued`, before delivery is known) would make the
    // run resolve with no anchor and fall out to the standalone-run render
    // path (issue-detail.tsx's `item.kind === "run"` timeline branch, gated
    // by nothing but timeline order) instead of the root-anchored path this
    // test exists to exercise (comment-card.tsx's `{open && ...}` block) —
    // an unanchored completed run would trivially "pass" a bare
    // Stop-button-gone assertion while proving nothing about the collapse
    // pin. Assert the node's absence directly (not just the Stop button) so
    // a stale intermediate render, where completion has landed but the
    // collapse re-close hasn't yet, can't pass this assertion.
    const completed: AgentTask = {
      ...task, status: "completed", completed_at: "2026-01-16T00:06:00Z", delivered_comment_ids: [root.id],
    };
    mockApiObj.listTasksByIssue.mockResolvedValue([completed]);
    act(() => {
      client.setQueryData(issueKeys.tasks("issue-1"), [completed]);
    });
    await waitFor(() => expect(container.querySelector(`[data-run-id="${taskId}"]`)).toBeNull());
    expect(mockCollapseStoreState.isCollapsed(root.id)).toBe(true);
  });

  it("latches a thread's length-expanded preference when a target reveal forces it open, surviving after the target releases", async () => {
    const root = mockTimeline[0]!;
    const replies: TimelineEntry[] = Array.from({ length: 4 }, (_, i) => ({
      ...mockTimeline[1]!,
      id: `latch-target-reply-${i}`,
      parent_id: root.id,
      content: `Target reply ${i}`,
      created_at: `2026-01-16T00:0${i}:00Z`,
    }));
    mockApiObj.listTimeline.mockResolvedValue([root, ...replies]);
    const queryClient = createTestQueryClient();
    // Built directly (not via renderIssueDetailWithHighlight) so the target
    // reveal can actually "release" by rerendering with highlightCommentId
    // cleared — 01-DESIGN "Target reveal" row: "replacement/cancellation/view
    // exit releases it." targetRootId is purely a function of this prop; it
    // has no internal timeout, so releasing it means the caller (here, the
    // rerender) stops passing it, exactly like a consumed deep link.
    const { rerender } = render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={queryClient}>
          <IssueDetail issueId="issue-1" highlightCommentId={replies[0]!.id} />
        </QueryClientProvider>
      </I18nProvider>,
    );

    // The target (the oldest, compact-window-hidden reply) is revealed.
    await screen.findByText("Target reply 0");
    expect(useIssueDisclosureStore.getState().expandedThreadIdsByIssue["issue-1"]?.has(root.id)).toBe(true);

    // The deep link is consumed — the caller stops passing highlightCommentId,
    // releasing the pin (targetRootId becomes null).
    rerender(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={queryClient}>
          <IssueDetail issueId="issue-1" />
        </QueryClientProvider>
      </I18nProvider>,
    );

    // This is finding 1's regression: without the fix, targetRootId's pin
    // drops with nothing having latched it, and the thread silently refolds,
    // hiding the reply the user was just shown.
    await waitFor(() => expect(screen.queryByText("Target reply 0")).toBeInTheDocument());
    expect(useIssueDisclosureStore.getState().expandedThreadIdsByIssue["issue-1"]?.has(root.id)).toBe(true);
  });

  it("does not latch a thread's length preference merely because in-page find opened and closed", async () => {
    // find.open must force a compact thread open (so its content is
    // searchable) WITHOUT writing anything durable — 01-DESIGN "Find open /
    // close" row: "no write to any fold preference." This is the negative
    // control that keeps the target-reveal latch from over-latching.
    const root = mockTimeline[0]!;
    const replies: TimelineEntry[] = Array.from({ length: 4 }, (_, i) => ({
      ...mockTimeline[1]!,
      id: `latch-find-reply-${i}`,
      parent_id: root.id,
      content: `Find reply ${i}`,
      created_at: `2026-01-16T00:0${i}:00Z`,
    }));
    mockApiObj.listTimeline.mockResolvedValue([root, ...replies]);
    // useInPageFind's Cmd/Ctrl+F handler gates on the container having a
    // non-empty getClientRects() — real in a browser, always empty in jsdom.
    // Scoped to this test only: no other test in this file drives find.open,
    // and stubbing it globally risks masking an unrelated visibility bug in
    // a future test.
    const originalGetClientRects = Element.prototype.getClientRects;
    Element.prototype.getClientRects = function (this: Element) {
      return [{ width: 1, height: 1 }] as unknown as DOMRectList;
    };
    try {
      renderIssueDetail();
      await screen.findByText("Find reply 3");
      expect(screen.queryByText("Find reply 0")).not.toBeInTheDocument();

      fireEvent.keyDown(document, { key: "f", ctrlKey: true });
      // find.open forces the thread flat/open for searchability.
      await screen.findByText("Find reply 0");
      // The pin is doing the work here — nothing has latched yet.
      expect(useIssueDisclosureStore.getState().expandedThreadIdsByIssue["issue-1"]?.has(root.id)).toBeFalsy();

      // Escape is handled by FindBar's own input, not a global listener.
      const findInput = screen.getByPlaceholderText("Find in issue...");
      fireEvent.keyDown(findInput, { key: "Escape" });

      // Closing find drops the pin. Per row 58, no durable entry was ever
      // written for this root, so it refolds — this is the correct, intended
      // behavior for find (unlike the draft/run/target latches above).
      await waitFor(() => expect(screen.queryByText("Find reply 0")).not.toBeInTheDocument());
      expect(useIssueDisclosureStore.getState().expandedThreadIdsByIssue["issue-1"]?.has(root.id)).toBeFalsy();
    } finally {
      Element.prototype.getClientRects = originalGetClientRects;
    }
  });

  // CHE-436: 01-DESIGN "Find and target reveal lifecycle" — find must reveal
  // EVERY fold class, not just manual collapse/length (already covered by
  // "does not latch..." above and the pre-existing forceThreadOpen tests).
  // This closes the gap CHE-436 was scoped to fix: a resolved-bar root and a
  // reply-resolution conclusion fold both stayed folded under find before
  // this change, because `expandedResolvedIds`/`flattenGroups` read the raw
  // (non-overridden) resolved-expand store.
  describe("find reveals resolved-thread folds (CHE-436)", () => {
    async function openFind() {
      const originalGetClientRects = Element.prototype.getClientRects;
      Element.prototype.getClientRects = function (this: Element) {
        return [{ width: 1, height: 1 }] as unknown as DOMRectList;
      };
      fireEvent.keyDown(document, { key: "f", ctrlKey: true });
      return () => {
        Element.prototype.getClientRects = originalGetClientRects;
      };
    }

    it("reveals a resolved root (folded to a bar) while find is open, and refolds it on close", async () => {
      const resolvedRoot: TimelineEntry = {
        ...mockTimeline[0]!,
        id: "resolved-root",
        content: "Resolved root body — findable sentinel",
        resolved_at: "2026-01-19T00:00:00Z",
      };
      mockApiObj.listTimeline.mockResolvedValue([resolvedRoot]);
      renderIssueDetail();

      // Folded to a bar by default — the body text is not in the DOM, only
      // the "N resolved comment(s) from ..." bar button.
      await screen.findByRole("button", { name: /resolved comment/ });
      expect(screen.queryByText("Resolved root body — findable sentinel")).not.toBeInTheDocument();

      const restoreGetClientRects = await openFind();
      try {
        await screen.findByText("Resolved root body — findable sentinel");
        // Row 58: find never writes the resolved-expand preference.
        expect(useResolvedExpandStore.getState().expandedByIssue["issue-1"]?.has("resolved-root")).toBeFalsy();

        const findInput = screen.getByPlaceholderText("Find in issue...");
        fireEvent.keyDown(findInput, { key: "Escape" });

        await waitFor(() =>
          expect(screen.queryByText("Resolved root body — findable sentinel")).not.toBeInTheDocument(),
        );
        expect(useResolvedExpandStore.getState().expandedByIssue["issue-1"]?.has("resolved-root")).toBeFalsy();
      } finally {
        restoreGetClientRects();
      }
    });

    it("reveals a reply-resolution conclusion fold (other replies hidden behind it) while find is open", async () => {
      const root = { ...mockTimeline[0]!, id: "concl-root", content: "Conclusion root" };
      const hiddenReply: TimelineEntry = {
        ...mockTimeline[1]!,
        id: "concl-hidden",
        parent_id: root.id,
        content: "Hidden middle reply — findable sentinel",
        created_at: "2026-01-16T00:01:00Z",
      };
      const resolutionReply: TimelineEntry = {
        ...mockTimeline[1]!,
        id: "concl-resolution",
        parent_id: root.id,
        content: "The resolution reply",
        created_at: "2026-01-16T00:02:00Z",
        resolved_at: "2026-01-17T00:00:00Z",
      };
      mockApiObj.listTimeline.mockResolvedValue([root, hiddenReply, resolutionReply]);
      renderIssueDetail();

      await screen.findByText("The resolution reply");
      expect(screen.queryByText("Hidden middle reply — findable sentinel")).not.toBeInTheDocument();

      const restoreGetClientRects = await openFind();
      try {
        await screen.findByText("Hidden middle reply — findable sentinel");
        expect(useResolvedExpandStore.getState().expandedByIssue["issue-1"]?.has(root.id)).toBeFalsy();

        const findInput = screen.getByPlaceholderText("Find in issue...");
        fireEvent.keyDown(findInput, { key: "Escape" });

        await waitFor(() =>
          expect(screen.queryByText("Hidden middle reply — findable sentinel")).not.toBeInTheDocument(),
        );
      } finally {
        restoreGetClientRects();
      }
    });

    it("disables the manual Collapse control on an open thread while find is open, with a localized reason, and re-enables it on close", async () => {
      const root = mockTimeline[0]!;
      const { container } = renderIssueDetail();
      await screen.findByText("Started working on this");

      const rootWrapper = () => container.querySelector(`#comment-${root.id}`) as HTMLElement;
      const collapseButton = () => within(rootWrapper()).getByRole("button", { name: "Collapse thread" });
      expect(collapseButton()).toBeEnabled();

      const restoreGetClientRects = await openFind();
      try {
        await waitFor(() => expect(collapseButton()).toBeDisabled());
        expect(collapseButton()).toHaveAttribute(
          "title",
          "Can't collapse while find is open",
        );
        // The manual-collapse store itself is untouched — this is a UI-level
        // disable, not a state write.
        expect(mockCollapseStoreState.isCollapsed(root.id)).toBe(false);

        const findInput = screen.getByPlaceholderText("Find in issue...");
        fireEvent.keyDown(findInput, { key: "Escape" });
        await waitFor(() => expect(collapseButton()).toBeEnabled());
      } finally {
        restoreGetClientRects();
      }
    });

    it("reveals the collapsed description while find is open, without writing the description-expanded preference", async () => {
      mockApiObj.getIssue.mockResolvedValue({
        ...mockIssue,
        description: "A short description",
      });
      renderIssueDetail();
      await screen.findByText("A short description");

      const restoreGetClientRects = await openFind();
      try {
        // A short description never becomes collapsed/inert (description-
        // disclosure.tsx's own "canDisclose" gate) — assert the SHARED
        // reveal wiring at least doesn't regress: the description stays
        // visible and the store is never written while find is open.
        await waitFor(() => expect(screen.getByText("A short description")).toBeInTheDocument());
        expect(useIssueDisclosureStore.getState().descriptionExpandedIssueIds.has("issue-1")).toBe(false);
      } finally {
        restoreGetClientRects();
      }
    });
  });

  // Sol's CHE-436 PR #43 review, blocking finding 2: closing find with no
  // active match (no query, or a query with zero matches) must restore the
  // PRE-open row to its pre-open offset within the scroll container, not
  // re-center it — 01-DESIGN.md:74 "preserve offset where possible". Before
  // the fix, the entry anchor was captured in an effect keyed on
  // `[find.open]`, which only runs AFTER the same render already flattened
  // every fold open — so the "pre-open" snapshot was actually a post-reveal
  // one, and the restore always re-centered regardless.
  it("restores the pre-open row to its original offset (not centered) when find closes with no active match", async () => {
    const root = mockTimeline[0]!;
    mockApiObj.listTimeline.mockResolvedValue([root]);

    // Every element reports the same fixed size; only the root row's THIS
    // test cares about, and it is deliberately NOT at the position centering
    // would produce (which depends on container height/2), so a passing
    // "top === originalTop - scrollDelta" assertion below could not be
    // satisfied by the old centering math except by coincidence.
    const originalGetClientRects = Element.prototype.getClientRects;
    const originalGetBoundingClientRect = Element.prototype.getBoundingClientRect;
    Element.prototype.getClientRects = function (this: Element) {
      return [{ width: 1, height: 1 }] as unknown as DOMRectList;
    };
    // Container top pinned at 0, height 400. Root row starts 130px from the
    // container's top edge before find opens, and (since nothing in the
    // timeline changes shape here) stays there for the rest of the test —
    // scrollTop deltas are what the restore effect must apply on top of this
    // fixed layout to reproduce the original 130px offset after any
    // intervening scroll.
    let containerScrollTop = 0;
    Element.prototype.getBoundingClientRect = function (this: Element) {
      if (this.hasAttribute("data-issue-timeline-scroll")) {
        return { top: 0, bottom: 400, height: 400, left: 0, right: 800, width: 800 } as DOMRect;
      }
      if (this.id === `comment-${root.id}`) {
        return { top: 130 - containerScrollTop, bottom: 160 - containerScrollTop, height: 30, left: 0, right: 800, width: 800 } as DOMRect;
      }
      return originalGetBoundingClientRect.call(this);
    };

    try {
      renderIssueDetail();
      await screen.findByText("Started working on this");

      const container = document.querySelector("[data-issue-timeline-scroll]") as HTMLElement;
      Object.defineProperty(container, "scrollTop", {
        get: () => containerScrollTop,
        set: (v: number) => { containerScrollTop = v; },
        configurable: true,
      });

      fireEvent.keyDown(document, { key: "f", ctrlKey: true });
      await screen.findByPlaceholderText("Find in issue...");

      // Simulate the user scrolling away from the entry row WHILE find is
      // open (no query typed — the no-match/never-searched path 01-DESIGN
      // calls out explicitly). The entry anchor was captured before this
      // scroll happened (pre-open), so its target offset (130) predates it.
      containerScrollTop = 500;

      const findInput = screen.getByPlaceholderText("Find in issue...");
      fireEvent.keyDown(findInput, { key: "Escape" });
      await waitFor(() => expect(screen.queryByPlaceholderText("Find in issue...")).not.toBeInTheDocument());
      await act(async () => {
        await new Promise((resolve) => requestAnimationFrame(resolve));
        await new Promise((resolve) => requestAnimationFrame(resolve));
      });

      // Restore must move scrollTop back toward 0 so the root row's offset
      // returns to its captured pre-open value (130px from the container
      // top) — NOT leave it at the scrolled-away 500, and NOT center it
      // (centering in a 400px container against a 30px-tall row targets
      // scrollTop ≈ 500 + 130 - 185 = 445, a value this assertion also
      // rules out).
      await waitFor(() => {
        expect(containerScrollTop).toBeCloseTo(0, 0);
      });
    } finally {
      Element.prototype.getClientRects = originalGetClientRects;
      Element.prototype.getBoundingClientRect = originalGetBoundingClientRect;
    }
  });

  // CHE-476 (CHE-380 Gap A): restores 01-DESIGN line 51's original scope,
  // narrowed out of PR #31/CHE-435 because bare focus/selection with no
  // unsaved change produces no useCommentDraftStore entry —
  // `rootIdsWithActiveReplyDraft` above genuinely cannot see it (see its
  // comment). `rootIdsWithActiveFocusOrSelection` closes that gap by reading
  // `document.activeElement` / `document.getSelection()` directly against
  // each thread root's DOM subtree.
  it("refuses Show less while the thread being collapsed contains the focused element, with no unsaved draft", async () => {
    const root = mockTimeline[0]!;
    const replies: TimelineEntry[] = Array.from({ length: 4 }, (_, i) => ({
      ...mockTimeline[1]!,
      id: `focus-reply-${i}`,
      parent_id: root.id,
      content: `Focus reply ${i}`,
      created_at: `2026-01-16T00:0${i}:00Z`,
    }));
    mockApiObj.listTimeline.mockResolvedValue([root, ...replies]);
    const { container } = renderIssueDetail();
    await screen.findByText("Focus reply 3");
    // Expand past the compact window — Show less only renders once expanded.
    fireEvent.click(await screen.findByRole("button", { name: /Show \d+ more repl/ }));
    await screen.findByRole("button", { name: "Show less" });

    // Focus something inside this thread's subtree that carries no draft —
    // the reply composer's placeholder input itself, before any typing.
    // The composer starts as a lazy shell; activate it first so the real
    // editor mounts.
    const threadWrapper = container.querySelector(`#comment-${root.id}`) as HTMLElement;
    fireEvent.click(within(threadWrapper).getByTestId("reply-composer-shell"));
    const replyBox = await within(threadWrapper).findByPlaceholderText("Leave a reply...");
    act(() => {
      (replyBox as HTMLElement).focus();
      fireEvent.focusIn(replyBox);
    });

    // No draft content was typed, so rootIdsWithActiveReplyDraft does not
    // cover this — only the focus signal does. Show less must be withheld.
    await waitFor(() => expect(screen.queryByRole("button", { name: "Show less" })).not.toBeInTheDocument());

    // Blurring releases the pin — Show less returns.
    act(() => {
      (replyBox as HTMLElement).blur();
      fireEvent.focusOut(replyBox);
    });
    await waitFor(() => expect(screen.getByRole("button", { name: "Show less" })).toBeInTheDocument());
  });

  it("refuses Show less while the thread being collapsed contains a non-collapsed text selection, with no unsaved draft", async () => {
    const root = mockTimeline[0]!;
    const replies: TimelineEntry[] = Array.from({ length: 4 }, (_, i) => ({
      ...mockTimeline[1]!,
      id: `sel-reply-${i}`,
      parent_id: root.id,
      content: `Sel reply ${i}`,
      created_at: `2026-01-16T00:0${i}:00Z`,
    }));
    mockApiObj.listTimeline.mockResolvedValue([root, ...replies]);
    const { container } = renderIssueDetail();
    await screen.findByText("Sel reply 3");
    fireEvent.click(await screen.findByRole("button", { name: /Show \d+ more repl/ }));
    await screen.findByRole("button", { name: "Show less" });

    const threadWrapper = container.querySelector(`#comment-${root.id}`) as HTMLElement;
    const textNode = within(threadWrapper).getByText("Sel reply 3").firstChild as Node;
    const range = document.createRange();
    range.selectNodeContents(textNode);
    const sel = window.getSelection()!;
    act(() => {
      sel.removeAllRanges();
      sel.addRange(range);
      fireEvent(document, new Event("selectionchange"));
    });

    // AC 2: a real range with no unsaved change still blocks Show less.
    await waitFor(() => expect(screen.queryByRole("button", { name: "Show less" })).not.toBeInTheDocument());

    act(() => {
      sel.removeAllRanges();
      fireEvent(document, new Event("selectionchange"));
    });
    await waitFor(() => expect(screen.getByRole("button", { name: "Show less" })).toBeInTheDocument());
  });

  it("refuses Show less for a cross-boundary selection anchored inside the collapsing thread but extending outside it", async () => {
    // Terra review (CHE-476 PR #36): commonAncestorContainer is the wrong
    // containment test — a selection that starts in-thread and ends outside
    // it has a common ancestor ABOVE both roots, so the old check missed it
    // and Show less would hide the anchored text. anchorNode containment
    // (01-DESIGN-v3.md:51: refusal keys off the Selection's anchor) fixes it.
    const root = mockTimeline[0]!;
    const otherRoot: TimelineEntry = { ...mockTimeline[1]!, id: "boundary-other-root", parent_id: null, content: "Other root", created_at: "2026-01-16T00:10:00Z" };
    const replies: TimelineEntry[] = Array.from({ length: 4 }, (_, i) => ({
      ...mockTimeline[1]!,
      id: `boundary-reply-${i}`,
      parent_id: root.id,
      content: `Boundary reply ${i}`,
      created_at: `2026-01-16T00:0${i}:00Z`,
    }));
    mockApiObj.listTimeline.mockResolvedValue([root, ...replies, otherRoot]);
    const { container } = renderIssueDetail();
    await screen.findByText("Boundary reply 3");
    await screen.findByText("Other root");
    fireEvent.click(await screen.findByRole("button", { name: /Show \d+ more repl/ }));
    await screen.findByRole("button", { name: "Show less" });

    const threadWrapper = container.querySelector(`#comment-${root.id}`) as HTMLElement;
    const otherWrapper = container.querySelector(`#comment-${otherRoot.id}`) as HTMLElement;
    const anchorNode = within(threadWrapper).getByText("Boundary reply 3").firstChild as Node;
    const focusNode = within(otherWrapper).getByText("Other root").firstChild as Node;
    const sel = window.getSelection()!;
    act(() => {
      sel.removeAllRanges();
      sel.setBaseAndExtent(anchorNode, 0, focusNode, 1);
      fireEvent(document, new Event("selectionchange"));
    });

    // The selection's commonAncestorContainer sits above both roots, but its
    // anchor is still inside root's subtree — Show less must stay withheld.
    await waitFor(() => expect(screen.queryByRole("button", { name: "Show less" })).not.toBeInTheDocument());

    act(() => {
      sel.removeAllRanges();
      fireEvent(document, new Event("selectionchange"));
    });
    await waitFor(() => expect(screen.getByRole("button", { name: "Show less" })).toBeInTheDocument());
  });

  it("does not block Show less on a thread when focus is inside a different thread that is not being collapsed", async () => {
    const root = mockTimeline[0]!;
    const otherRoot: TimelineEntry = { ...mockTimeline[1]!, id: "other-root", parent_id: null, content: "Other root", created_at: "2026-01-16T00:10:00Z" };
    const replies: TimelineEntry[] = Array.from({ length: 4 }, (_, i) => ({
      ...mockTimeline[1]!,
      id: `scope-reply-${i}`,
      parent_id: root.id,
      content: `Scope reply ${i}`,
      created_at: `2026-01-16T00:0${i}:00Z`,
    }));
    mockApiObj.listTimeline.mockResolvedValue([root, ...replies, otherRoot]);
    const { container } = renderIssueDetail();
    await screen.findByText("Scope reply 3");
    await screen.findByText("Other root");
    fireEvent.click(await screen.findByRole("button", { name: /Show \d+ more repl/ }));
    const showLess = await screen.findByRole("button", { name: "Show less" });

    // Focus lands in the OTHER thread's own reply composer — AC 3 requires
    // the refusal to stay scoped to the subtree actually being collapsed.
    const otherWrapper = container.querySelector(`#comment-${otherRoot.id}`) as HTMLElement;
    fireEvent.click(within(otherWrapper).getByTestId("reply-composer-shell"));
    const otherReplyBox = await within(otherWrapper).findByPlaceholderText("Leave a reply...");
    act(() => {
      (otherReplyBox as HTMLElement).focus();
      fireEvent.focusIn(otherReplyBox);
    });

    // root's own Show less must remain available throughout.
    await waitFor(() => expect(screen.getByRole("button", { name: "Show less" })).toBe(showLess));
  });

  it("does not block Show less for a collapsed caret-only selection with no unsaved draft", async () => {
    const root = mockTimeline[0]!;
    const replies: TimelineEntry[] = Array.from({ length: 4 }, (_, i) => ({
      ...mockTimeline[1]!,
      id: `caret-reply-${i}`,
      parent_id: root.id,
      content: `Caret reply ${i}`,
      created_at: `2026-01-16T00:0${i}:00Z`,
    }));
    mockApiObj.listTimeline.mockResolvedValue([root, ...replies]);
    const { container } = renderIssueDetail();
    await screen.findByText("Caret reply 3");
    fireEvent.click(await screen.findByRole("button", { name: /Show \d+ more repl/ }));
    const showLess = await screen.findByRole("button", { name: "Show less" });

    const threadWrapper = container.querySelector(`#comment-${root.id}`) as HTMLElement;
    const textNode = within(threadWrapper).getByText("Caret reply 3").firstChild as Node;
    const range = document.createRange();
    range.setStart(textNode, 0);
    range.collapse(true);
    const sel = window.getSelection()!;
    act(() => {
      sel.removeAllRanges();
      sel.addRange(range);
      fireEvent(document, new Event("selectionchange"));
    });

    // AC 4: a caret (isCollapsed === true) is not a "selection" for this
    // purpose — Show less must remain available.
    await waitFor(() => expect(screen.getByRole("button", { name: "Show less" })).toBe(showLess));
  });

  it("replaces each queued run in place without moving replies behind later requests", async () => {
    const root = mockTimeline[0]!;
    const second = { ...root, id: "request-two", parent_id: root.id, content: "Second request", created_at: "2026-01-16T00:00:02Z" };
    const third = { ...root, id: "request-three", parent_id: root.id, content: "Third request", created_at: "2026-01-16T00:00:04Z" };
    const tasks: AgentTask[] = [root, second, third].map((trigger, index) => ({
      id: `ba2e8d1c-7f9b-4e2a-9c1d-123456789ab${index}`, agent_id: "agent-1", runtime_id: "runtime-1", issue_id: "issue-1",
      status: "queued", priority: 0, created_at: trigger.created_at,
      started_at: null, dispatched_at: null, completed_at: null, result: null, error: null,
      trigger_comment_id: trigger.id, delivered_comment_ids: [],
    }));
    let timeline = [root, second, third];
    mockApiObj.listTimeline.mockResolvedValue(timeline);
    mockApiObj.listTasksByIssue.mockResolvedValue(tasks);
    const client = createTestQueryClient();
    const { container } = render(<I18nProvider locale="en" resources={TEST_RESOURCES}>
      <QueryClientProvider client={client}><IssueDetail issueId="issue-1" defaultSidebarOpen={false} /></QueryClientProvider>
    </I18nProvider>);
    await waitFor(() => expect(container.querySelectorAll('[data-run-slot-id]')).toHaveLength(3));
    const slots = tasks.map((task) => container.querySelector(`[data-run-slot-id="${task.id}"]`)!);
    fireEvent.click(within(slots[0] as HTMLElement).getByRole("button", { name: /View activity/ }));
    await within(slots[0] as HTMLElement).findByText("No activity recorded yet.");
    for (const index of [0, 1]) {
      const reply: TimelineEntry = { ...mockTimeline[1]!, id: `answer-${index}`, parent_id: index === 0 ? null : root.id,
        source_task_id: tasks[index]!.id, content: `Answer ${index}`, created_at: `2026-01-16T00:01:0${index}Z` };
      tasks[index] = { ...tasks[index]!, status: "completed", completed_at: reply.created_at,
        delivered_comment_ids: [tasks[index]!.trigger_comment_id!] };
      timeline = [...timeline, reply];
      mockApiObj.listTasksByIssue.mockResolvedValue([...tasks]);
      mockApiObj.listTimeline.mockResolvedValue(timeline);
      act(() => {
        client.setQueryData(issueKeys.tasks("issue-1"), [...tasks]);
        client.setQueryData(issueKeys.timeline("issue-1"), timeline);
      });
      await screen.findByText(reply.content!);
      // task 0's reply (answer-0) is projected onto root itself (comment-runs.ts
      // rewrites its parent_id to root.id since its anchor IS the root) — this
      // run keeps the root-anchored AgentRunComment identity, so its DOM node
      // is reused in place (`toBe(slots[0])`) exactly as before.
      //
      // task 1's reply (answer-1) is a SIBLING nested reply of root, distinct
      // from the "request-two" comment task 1 was originally anchored to
      // (queued, pre-reply) — projectThreadDisplay is the sole thread-display
      // input (01-DESIGN "Thread selection and run placement") and gives every
      // published reply its own chronological slot, so the run's controls
      // relocate from their prior post-"request-two" run-only slot to
      // answer-1's own slot. A new DOM node at the new position is the
      // intended v2 behavior, not a regression — the old anchor-recursion
      // model's DOM-identity-across-relocation guarantee is exactly what
      // 01-DESIGN's per-reply chronological slotting replaces.
      if (index === 0) {
        expect(container.querySelector(`[data-run-slot-id="${tasks[index]!.id}"]`)).toBe(slots[index]);
      } else {
        const relocated = container.querySelector(`#comment-${reply.id}`)!;
        expect(relocated).not.toBeNull();
        expect(relocated.querySelector(`[data-run-id="${tasks[index]!.id}"]`)).not.toBeNull();
        slots[index] = relocated;
      }
      expect(slots[index]!.textContent).toContain(reply.content);
      expect(container.querySelectorAll(`[data-run-id="${tasks[index]!.id}"]`)).toHaveLength(1);
    }
    expect(within(slots[0] as HTMLElement).getByRole("button", { name: "Open full log" })).toBeInTheDocument();
    expect(within(slots[0] as HTMLElement).queryByRole("button", { name: /View activity/ })).not.toBeInTheDocument();
    expect(slots[0]!.nextElementSibling?.id).toBe("comment-request-two");
    expect(container.querySelector(`[data-run-slot-id="${tasks[2]!.id}"]`)).toBe(slots[2]);
    expect(within(slots[2] as HTMLElement).getByText("Waiting for an available agent.")).toBeInTheDocument();
  });

  it("keeps a downstream run in the thread after its triggering agent reply is projected there", async () => {
    const root = mockTimeline[0]!;
    const first: AgentTask = { id: "ba2e8d1c-7f9b-4e2a-9c1d-123456789ab0", agent_id: "agent-1", runtime_id: "runtime-1", issue_id: "issue-1",
      status: "completed", priority: 0, created_at: root.created_at, started_at: root.created_at, dispatched_at: null,
      completed_at: "2026-01-16T00:01:00Z", result: null, error: null, trigger_comment_id: root.id };
    const answer = { ...mockTimeline[1]!, id: "answer-a", parent_id: null, source_task_id: first.id, content: "Agent A response" };
    const second: AgentTask = { ...first, id: "ba2e8d1c-7f9b-4e2a-9c1d-123456789ab1", agent_id: "agent-2", status: "queued",
      started_at: null, completed_at: null, trigger_comment_id: answer.id };
    mockApiObj.listTimeline.mockResolvedValue([root, answer]);
    mockApiObj.listTasksByIssue.mockResolvedValue([first, second]);
    const { container } = renderIssueDetail();
    await screen.findByText(answer.content);
    await waitFor(() => expect(container.querySelectorAll(`[data-run-id="${second.id}"]`)).toHaveLength(1));
    expect(container.querySelector(`#comment-${root.id}`)?.querySelector(`[data-run-id="${second.id}"]`)).not.toBeNull();
  });

  it.each(["failed", "cancelled"] as const)("keeps a %s run outside the user reply that triggered it", async (status) => {
    const root = mockTimeline[0]!;
    const trigger = { ...root, id: "user-reply", parent_id: root.id, content: "Please try this task" };
    const task: AgentTask = {
      id: "4a2e8d1c-7f9b-4e2a-9c1d-123456789abc", agent_id: "agent-1", runtime_id: "runtime-1", issue_id: "issue-1",
      status, priority: 0, created_at: "2026-01-16T00:00:00Z",
      started_at: null, dispatched_at: null, completed_at: "2026-01-16T00:01:00Z", result: null, error: null,
      trigger_comment_id: trigger.id, delivered_comment_ids: [],
    };
    mockApiObj.listTimeline.mockResolvedValue([root, trigger]);
    mockApiObj.listTasksByIssue.mockResolvedValue([task]);
    const { container } = renderIssueDetail();
    await screen.findByText(trigger.content);
    await waitFor(() => expect(container.querySelector(`[data-run-comment-id="${task.id}"]`)).not.toBeNull());
    const userReply = container.querySelector("#comment-user-reply")!;
    const agentBlock = container.querySelector(`[data-run-comment-id="${task.id}"]`)!;
    expect(userReply.contains(agentBlock)).toBe(false);
    expect(userReply.nextElementSibling).toBe(agentBlock);
    expect(within(agentBlock as HTMLElement).getByRole("button", { name: "Retry run" })).toBeInTheDocument();
    expect(container.querySelectorAll(`[data-run-id="${task.id}"]`)).toHaveLength(1);
  });

  // Details is creator + immutable timestamps, so it ranks below the
  // execution log, which is what people actually open the sidebar for.
  it("orders the Details section after the execution log", async () => {
    mockApiObj.listTasksByIssue.mockResolvedValue([
      {
        id: "task-past",
        agent_id: "agent-1",
        runtime_id: "runtime-1",
        issue_id: "issue-1",
        status: "completed",
        priority: 0,
        dispatched_at: null,
        started_at: "2026-06-08T08:00:00Z",
        completed_at: "2026-06-08T08:05:00Z",
        result: null,
        error: null,
        created_at: "2026-06-08T08:00:00Z",
        trigger_summary: "Started from comment",
      },
    ]);

    renderIssueDetail();

    const executionLog = await screen.findByText("Execution log");
    const details = screen.getByText("Details");

    // DOCUMENT_POSITION_FOLLOWING: Details comes after the execution log.
    expect(
      executionLog.compareDocumentPosition(details) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("shows 'not found' message when issue does not exist", async () => {
    mockApiObj.getIssue.mockRejectedValue(new Error("Not found"));

    renderIssueDetail("nonexistent-id");

    await waitFor(() => {
      expect(
        screen.getByText("This issue does not exist or has been deleted in this workspace."),
      ).toBeInTheDocument();
    });
  });

  it("shows 'Back' button when issue is not found and no onDelete prop", async () => {
    mockApiObj.getIssue.mockRejectedValue(new Error("Not found"));

    renderIssueDetail("nonexistent-id");

    await waitFor(() => {
      expect(screen.getByText("Back")).toBeInTheDocument();
    });
  });

  it("renders comments from timeline", async () => {
    renderIssueDetail();

    await waitFor(() => {
      expect(screen.getByText("Started working on this")).toBeInTheDocument();
    });

    expect(screen.getByText("I can help with this")).toBeInTheDocument();
  });

  it("prefers timeline identity when the actor is absent from the member directory", async () => {
    mockApiObj.listTimeline.mockResolvedValue([
      {
        type: "comment",
        id: "former-member-comment",
        actor_type: "member",
        actor_id: "former-user-1",
        actor_name: "Former Member",
        actor_avatar_url: "https://profiles.example.com/former.png",
        content: "Authored before leaving",
        parent_id: null,
        created_at: "2026-01-18T00:00:00Z",
        updated_at: "2026-01-18T00:00:00Z",
        comment_type: "comment",
      },
    ]);

    renderIssueDetail();

    await screen.findByText("Authored before leaving");
    expect(screen.getByText("Former Member")).toBeInTheDocument();
  });

  it("reruns the source task from an agent failure comment", async () => {
    mockApiObj.listTimeline.mockResolvedValue([
      ...mockTimeline,
      {
        type: "comment",
        id: "comment-failed-task",
        actor_type: "agent",
        actor_id: "agent-1",
        content: "API Error: 500 Internal server error",
        parent_id: null,
        created_at: "2026-01-18T00:00:00Z",
        updated_at: "2026-01-18T00:00:00Z",
        comment_type: "system",
        source_task_id: "task-failed",
      },
    ]);

    renderIssueDetail();

    await screen.findByText("API Error: 500 Internal server error");
    fireEvent.click(screen.getByRole("button", { name: "Retry run" }));

    await waitFor(() => {
      expect(mockApiObj.rerunIssue).toHaveBeenCalledWith("issue-1", "task-failed");
    });
  });

  it("does not show retry for child-done system comments", async () => {
    mockApiObj.listTimeline.mockResolvedValue([
      ...mockTimeline,
      {
        type: "comment",
        id: "comment-child-done",
        actor_type: "system",
        actor_id: "00000000-0000-0000-0000-000000000000",
        content: "Sub-issue MUL-123 is done.",
        parent_id: null,
        created_at: "2026-01-18T00:00:00Z",
        updated_at: "2026-01-18T00:00:00Z",
        comment_type: "system",
      },
    ]);

    renderIssueDetail();

    await screen.findByText("Sub-issue MUL-123 is done.");
    expect(screen.queryByRole("button", { name: "Retry run" })).not.toBeInTheDocument();
  });

  it("does not show retry for successful agent task comments", async () => {
    mockApiObj.listTimeline.mockResolvedValue([
      ...mockTimeline,
      {
        type: "comment",
        id: "comment-successful-task",
        actor_type: "agent",
        actor_id: "agent-1",
        content: "Finished the requested work.",
        parent_id: null,
        created_at: "2026-01-18T00:00:00Z",
        updated_at: "2026-01-18T00:00:00Z",
        comment_type: "comment",
        source_task_id: "task-success",
      },
    ]);

    renderIssueDetail();

    await screen.findByText("Finished the requested work.");
    expect(screen.queryByRole("button", { name: "Retry run" })).not.toBeInTheDocument();
  });

  it("does not show retry for agent system comments without a source task", async () => {
    mockApiObj.listTimeline.mockResolvedValue([
      ...mockTimeline,
      {
        type: "comment",
        id: "comment-agent-system",
        actor_type: "agent",
        actor_id: "agent-1",
        content: "System coordination update.",
        parent_id: null,
        created_at: "2026-01-18T00:00:00Z",
        updated_at: "2026-01-18T00:00:00Z",
        comment_type: "system",
      },
    ]);

    renderIssueDetail();

    await screen.findByText("System coordination update.");
    expect(screen.queryByRole("button", { name: "Retry run" })).not.toBeInTheDocument();
  });

  it("collapses non-trailing activity blocks and expands the last one by default", async () => {
    // Timeline shape:
    //   [activities: status_changed, priority_changed] ← block A (older)
    //   [comment-1]
    //   [activities: due_date_changed]                  ← block B (latest)
    // Block A should be collapsed; block B should be expanded.
    mockApiObj.listTimeline.mockResolvedValue([
      {
        type: "activity",
        id: "act-1",
        actor_type: "member",
        actor_id: "user-1",
        action: "status_changed",
        details: { from: "todo", to: "in_progress" },
        created_at: "2026-01-16T00:00:00Z",
      },
      {
        type: "activity",
        id: "act-2",
        actor_type: "member",
        actor_id: "user-1",
        action: "priority_changed",
        details: { from: "low", to: "high" },
        created_at: "2026-01-16T01:00:00Z",
      },
      {
        type: "comment",
        id: "comment-1",
        actor_type: "member",
        actor_id: "user-1",
        content: "Talking it through",
        parent_id: null,
        created_at: "2026-01-17T00:00:00Z",
        updated_at: "2026-01-17T00:00:00Z",
        comment_type: "comment",
      },
      {
        type: "activity",
        id: "act-3",
        actor_type: "member",
        actor_id: "user-1",
        action: "due_date_changed",
        details: { to: "2026-02-01T00:00:00Z" },
        created_at: "2026-01-18T00:00:00Z",
      },
    ] as TimelineEntry[]);

    renderIssueDetail();

    // Latest block (single activity) is expanded — its rendered text is visible.
    await waitFor(() => {
      expect(screen.getByText(/set due date to/i)).toBeInTheDocument();
    });

    // Older block is collapsed: shows the summary, hides the individual entries.
    expect(screen.getByText("2 activities")).toBeInTheDocument();
    expect(screen.queryByText(/changed status/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/changed priority/i)).not.toBeInTheDocument();

    // Clicking the summary expands the older block.
    fireEvent.click(screen.getByText("2 activities"));
    await waitFor(() => {
      expect(screen.getByText(/changed status/i)).toBeInTheDocument();
    });
    expect(screen.getByText(/changed priority/i)).toBeInTheDocument();
  });

  it("renders activity rows with unknown status values without crashing", async () => {
    mockApiObj.listTimeline.mockResolvedValue([
      {
        type: "activity",
        id: "act-unknown-status",
        actor_type: "member",
        actor_id: "user-1",
        action: "status_changed",
        details: { from: "todo", to: "mystery_status" },
        created_at: "2026-01-18T00:00:00Z",
      },
    ] as TimelineEntry[]);

    renderIssueDetail();

    await waitFor(() => {
      expect(screen.getByText(/from Todo to mystery_status/i)).toBeInTheDocument();
    });
  });

  // -------------------------------------------------------------------------
  // MUL-6413 — the activity glyph is per CATEGORY, so a move into a custom
  // status drew the icon of the built-in it sits beside: "In Review → Awaiting
  // Response" repainted identically and read as though nothing had moved.
  // Colour is what carries a custom status's own identity.
  // -------------------------------------------------------------------------

  const IN_REVIEW_BUILT_IN: IssueStatusEntry = {
    id: "in_review",
    workspace_id: "ws-1",
    key: "in_review",
    name: "In Review",
    description: "",
    category: "started",
    color: "#8b5cf6",
    is_system: true,
    position: 0,
    archived_at: null,
    created_at: "",
    updated_at: "",
  };

  const AWAITING_RESPONSE: IssueStatusEntry = {
    ...IN_REVIEW_BUILT_IN,
    id: "awaiting_response",
    key: "awaiting_response",
    name: "Awaiting Response",
    color: "#ff0000",
    is_system: false,
    position: 1,
  };

  function statusChangeIcon(to: string): SVGElement {
    const row = screen.getByText(new RegExp(`to ${to}$`, "i")).closest("div")
      ?.parentElement;
    const icon = row?.querySelector("svg");
    if (!icon) throw new Error(`no status glyph for the "${to}" activity row`);
    return icon;
  }

  it("paints a status-change activity in the custom status's own colour", async () => {
    mockApiObj.listTimeline.mockResolvedValue([
      {
        type: "activity",
        id: "act-custom-status",
        actor_type: "member",
        actor_id: "user-1",
        action: "status_changed",
        details: { from: "in_review", to: "awaiting_response" },
        created_at: "2026-01-18T00:00:00Z",
      },
    ] as TimelineEntry[]);

    renderIssueDetailWithStatusCatalog([IN_REVIEW_BUILT_IN, AWAITING_RESPONSE]);

    await waitFor(() => {
      expect(
        screen.getByText(/from In Review to Awaiting Response/i),
      ).toBeInTheDocument();
    });
    expect(statusChangeIcon("Awaiting Response").style.color).toBe("rgb(255, 0, 0)");
  });

  it("leaves a built-in status-change activity on its semantic token colour", async () => {
    // The catalog seeds a colour for the built-ins too, but those are theme
    // tokens in the UI — painting the seeded hex would hard-code one theme.
    mockApiObj.listTimeline.mockResolvedValue([
      {
        type: "activity",
        id: "act-built-in-status",
        actor_type: "member",
        actor_id: "user-1",
        action: "status_changed",
        details: { from: "in_progress", to: "in_review" },
        created_at: "2026-01-18T00:00:00Z",
      },
    ] as TimelineEntry[]);

    renderIssueDetailWithStatusCatalog([IN_REVIEW_BUILT_IN, AWAITING_RESPONSE]);

    await waitFor(() => {
      expect(screen.getByText(/from In Progress to In Review/i)).toBeInTheDocument();
    });
    const icon = statusChangeIcon("In Review");
    expect(icon.style.color).toBe("");
    expect(icon.getAttribute("class")).toContain("text-success");
  });

  it("truncates the trailing activity block to the most recent 8 entries with a show-more toggle", async () => {
    // 10 activities, all in the trailing block (no comment after them, so it's
    // the trailing block by definition). Alternating action types so the
    // 2-minute coalesce window never merges consecutive entries — we end up
    // with 10 distinct rows.
    const trailingBlock: TimelineEntry[] = [
      { type: "activity", id: "act-1", actor_type: "member", actor_id: "user-1", action: "status_changed", details: { from: "todo", to: "in_progress" }, created_at: "2026-01-18T00:00:00Z" },
      { type: "activity", id: "act-2", actor_type: "member", actor_id: "user-1", action: "priority_changed", details: { from: "low", to: "medium" }, created_at: "2026-01-18T00:01:00Z" },
      { type: "activity", id: "act-3", actor_type: "member", actor_id: "user-1", action: "status_changed", details: { from: "in_progress", to: "in_review" }, created_at: "2026-01-18T00:02:00Z" },
      { type: "activity", id: "act-4", actor_type: "member", actor_id: "user-1", action: "priority_changed", details: { from: "medium", to: "high" }, created_at: "2026-01-18T00:03:00Z" },
      { type: "activity", id: "act-5", actor_type: "member", actor_id: "user-1", action: "status_changed", details: { from: "in_review", to: "done" }, created_at: "2026-01-18T00:04:00Z" },
      { type: "activity", id: "act-6", actor_type: "member", actor_id: "user-1", action: "priority_changed", details: { from: "high", to: "urgent" }, created_at: "2026-01-18T00:05:00Z" },
      { type: "activity", id: "act-7", actor_type: "member", actor_id: "user-1", action: "status_changed", details: { from: "done", to: "blocked" }, created_at: "2026-01-18T00:06:00Z" },
      { type: "activity", id: "act-8", actor_type: "member", actor_id: "user-1", action: "priority_changed", details: { from: "urgent", to: "low" }, created_at: "2026-01-18T00:07:00Z" },
      { type: "activity", id: "act-9", actor_type: "member", actor_id: "user-1", action: "status_changed", details: { from: "blocked", to: "todo" }, created_at: "2026-01-18T00:08:00Z" },
      { type: "activity", id: "act-10", actor_type: "member", actor_id: "user-1", action: "due_date_changed", details: { to: "2026-02-01T00:00:00Z" }, created_at: "2026-01-18T00:09:00Z" },
    ] as TimelineEntry[];
    mockApiObj.listTimeline.mockResolvedValue(trailingBlock);

    renderIssueDetail();

    // In the truncated default state the "N activities" collapse header
    // stays hidden — the "Show N more" link is the only control we want
    // to expose for a glance at recent activity.
    await waitFor(() => {
      expect(screen.getByText("Show 2 more activities")).toBeInTheDocument();
    });
    expect(screen.queryByText("10 activities")).not.toBeInTheDocument();

    // Only the 8 most recent entries (act-3..act-10) are rendered by default.
    // act-1 and act-2 are folded behind the show-more line.
    expect(screen.getByText(/from In Progress to In Review/i)).toBeInTheDocument(); // act-3
    expect(screen.getByText(/set due date to/i)).toBeInTheDocument(); // act-10
    expect(screen.queryByText(/from Todo to In Progress/i)).not.toBeInTheDocument(); // act-1
    expect(screen.queryByText(/from Low to Medium/i)).not.toBeInTheDocument(); // act-2

    // Clicking the toggle reveals the older entries in place and brings the
    // full "N activities" header back (so the user can fold the block).
    fireEvent.click(screen.getByText("Show 2 more activities"));
    await waitFor(() => {
      expect(screen.getByText(/from Todo to In Progress/i)).toBeInTheDocument();
    });
    expect(screen.getByText(/from Low to Medium/i)).toBeInTheDocument();
    expect(screen.getByText(/set due date to/i)).toBeInTheDocument();
    expect(screen.getByText("10 activities")).toBeInTheDocument();
    expect(screen.queryByText(/Show \d+ more activit/i)).not.toBeInTheDocument();
  });

  it("does not show the show-more toggle when the trailing block has 8 or fewer entries", async () => {
    const trailingBlock: TimelineEntry[] = [
      { type: "activity", id: "act-1", actor_type: "member", actor_id: "user-1", action: "status_changed", details: { from: "todo", to: "in_progress" }, created_at: "2026-01-18T00:00:00Z" },
      { type: "activity", id: "act-2", actor_type: "member", actor_id: "user-1", action: "priority_changed", details: { from: "low", to: "high" }, created_at: "2026-01-18T00:01:00Z" },
      { type: "activity", id: "act-3", actor_type: "member", actor_id: "user-1", action: "status_changed", details: { from: "in_progress", to: "in_review" }, created_at: "2026-01-18T00:02:00Z" },
      { type: "activity", id: "act-4", actor_type: "member", actor_id: "user-1", action: "priority_changed", details: { from: "high", to: "urgent" }, created_at: "2026-01-18T00:03:00Z" },
      { type: "activity", id: "act-5", actor_type: "member", actor_id: "user-1", action: "status_changed", details: { from: "in_review", to: "done" }, created_at: "2026-01-18T00:04:00Z" },
      { type: "activity", id: "act-6", actor_type: "member", actor_id: "user-1", action: "priority_changed", details: { from: "urgent", to: "low" }, created_at: "2026-01-18T00:05:00Z" },
      { type: "activity", id: "act-7", actor_type: "member", actor_id: "user-1", action: "status_changed", details: { from: "done", to: "blocked" }, created_at: "2026-01-18T00:06:00Z" },
      { type: "activity", id: "act-8", actor_type: "member", actor_id: "user-1", action: "due_date_changed", details: { to: "2026-02-01T00:00:00Z" }, created_at: "2026-01-18T00:07:00Z" },
    ] as TimelineEntry[];
    mockApiObj.listTimeline.mockResolvedValue(trailingBlock);

    renderIssueDetail();

    await waitFor(() => {
      expect(screen.getByText("8 activities")).toBeInTheDocument();
    });
    // Every one of the 8 entries should be visible — the trailing block fits
    // exactly within the limit, so no "Show N more activities" line appears.
    expect(screen.getByText(/from Todo to In Progress/i)).toBeInTheDocument();
    expect(screen.getByText(/from Low to High/i)).toBeInTheDocument();
    expect(screen.getByText(/from In Progress to In Review/i)).toBeInTheDocument();
    expect(screen.getByText(/from High to Urgent/i)).toBeInTheDocument();
    expect(screen.getByText(/from In Review to Done/i)).toBeInTheDocument();
    expect(screen.getByText(/from Urgent to Low/i)).toBeInTheDocument();
    expect(screen.getByText(/from Done to Blocked/i)).toBeInTheDocument();
    expect(screen.getByText(/set due date to/i)).toBeInTheDocument();
    expect(screen.queryByText(/Show \d+ more activit/i)).not.toBeInTheDocument();
  });

  it("expanding a non-trailing block shows every entry — only the trailing block truncates older ones", async () => {
    // Non-trailing block (10 activities) + comment + trailing block (1 activity).
    // Manually expanding the older block must reveal all 10 entries — the
    // truncate-to-8 rule applies only to the trailing block.
    const timeline: TimelineEntry[] = [
      { type: "activity", id: "old-1", actor_type: "member", actor_id: "user-1", action: "status_changed", details: { from: "backlog", to: "todo" }, created_at: "2026-01-16T00:00:00Z" },
      { type: "activity", id: "old-2", actor_type: "member", actor_id: "user-1", action: "priority_changed", details: { from: "none", to: "low" }, created_at: "2026-01-16T00:01:00Z" },
      { type: "activity", id: "old-3", actor_type: "member", actor_id: "user-1", action: "status_changed", details: { from: "todo", to: "in_progress" }, created_at: "2026-01-16T00:02:00Z" },
      { type: "activity", id: "old-4", actor_type: "member", actor_id: "user-1", action: "priority_changed", details: { from: "low", to: "medium" }, created_at: "2026-01-16T00:03:00Z" },
      { type: "activity", id: "old-5", actor_type: "member", actor_id: "user-1", action: "status_changed", details: { from: "in_progress", to: "in_review" }, created_at: "2026-01-16T00:04:00Z" },
      { type: "activity", id: "old-6", actor_type: "member", actor_id: "user-1", action: "priority_changed", details: { from: "medium", to: "high" }, created_at: "2026-01-16T00:05:00Z" },
      { type: "activity", id: "old-7", actor_type: "member", actor_id: "user-1", action: "status_changed", details: { from: "in_review", to: "done" }, created_at: "2026-01-16T00:06:00Z" },
      { type: "activity", id: "old-8", actor_type: "member", actor_id: "user-1", action: "priority_changed", details: { from: "high", to: "urgent" }, created_at: "2026-01-16T00:07:00Z" },
      { type: "activity", id: "old-9", actor_type: "member", actor_id: "user-1", action: "status_changed", details: { from: "done", to: "blocked" }, created_at: "2026-01-16T00:08:00Z" },
      { type: "activity", id: "old-10", actor_type: "member", actor_id: "user-1", action: "priority_changed", details: { from: "urgent", to: "low" }, created_at: "2026-01-16T00:09:00Z" },
      {
        type: "comment", id: "comment-mid", actor_type: "member", actor_id: "user-1",
        content: "Splitting the blocks", parent_id: null,
        created_at: "2026-01-17T00:00:00Z", updated_at: "2026-01-17T00:00:00Z",
        comment_type: "comment",
      },
      { type: "activity", id: "last-1", actor_type: "member", actor_id: "user-1", action: "due_date_changed", details: { to: "2026-02-01T00:00:00Z" }, created_at: "2026-01-18T00:00:00Z" },
    ] as TimelineEntry[];
    mockApiObj.listTimeline.mockResolvedValue(timeline);

    renderIssueDetail();

    // The older block defaults to collapsed; its summary reports 10.
    await waitFor(() => {
      expect(screen.getByText("10 activities")).toBeInTheDocument();
    });
    // None of the older entries are rendered before expansion.
    expect(screen.queryByText(/from Backlog to Todo/i)).not.toBeInTheDocument();

    // Expand the older block by clicking its summary line.
    fireEvent.click(screen.getByText("10 activities"));

    // Every one of the 10 entries should now be visible — even though the
    // block has more than 8 entries, the truncate-to-8 rule does not apply
    // to non-trailing blocks, so no "Show N more activities" line appears.
    await waitFor(() => {
      expect(screen.getByText(/from Backlog to Todo/i)).toBeInTheDocument();
    });
    expect(screen.getByText(/from No priority to Low/i)).toBeInTheDocument();
    expect(screen.getByText(/from Todo to In Progress/i)).toBeInTheDocument();
    expect(screen.getByText(/from Low to Medium/i)).toBeInTheDocument();
    expect(screen.getByText(/from In Progress to In Review/i)).toBeInTheDocument();
    expect(screen.getByText(/from Medium to High/i)).toBeInTheDocument();
    expect(screen.getByText(/from In Review to Done/i)).toBeInTheDocument();
    expect(screen.getByText(/from High to Urgent/i)).toBeInTheDocument();
    expect(screen.getByText(/from Done to Blocked/i)).toBeInTheDocument();
    expect(screen.getByText(/from Urgent to Low/i)).toBeInTheDocument();
    expect(screen.queryByText(/Show \d+ more activit/i)).not.toBeInTheDocument();
  });

  describe("highlightCommentId scroll-to-comment", () => {
    it.each(["root", "reply"])("unfolds an assignment run with a resolved %s when a notification targets a hidden reply", async (resolved) => {
      const run: AgentTask = { id: "ba2e8d1c-7f9b-4e2a-9c1d-123456789abc", agent_id: "agent-1", runtime_id: "runtime-1", issue_id: "issue-1",
        status: "completed", priority: 0, created_at: "2026-01-16T00:00:00Z", started_at: null, dispatched_at: null,
        completed_at: "2026-01-16T00:01:00Z", result: null, error: null, delivered_comment_ids: [] };
      const root = { ...mockTimeline[1]!, id: "assigned-answer", parent_id: null, source_task_id: run.id,
        content: "Assignment answer", resolved_at: resolved === "root" ? "2026-01-17T00:00:00Z" : null };
      const target = { ...mockTimeline[0]!, id: "hidden-target", parent_id: root.id, content: "Hidden notification target" };
      const resolution = { ...target, id: "resolution", content: "Resolved reply", resolved_at: "2026-01-17T00:00:00Z" };
      mockApiObj.listTimeline.mockResolvedValue([root, target, ...(resolved === "reply" ? [resolution] : [])]);
      mockApiObj.listTasksByIssue.mockResolvedValue([run]);
      renderIssueDetailWithHighlight(target.id);
      await waitFor(() => expect(document.getElementById(`comment-${target.id}`)).not.toBeNull());
      await waitFor(() => expect(document.getElementById(`comment-${target.id}`)).toHaveClass(highlightedCommentBackgroundClass));
    });
    it("lands on the reply above a deleted one when a notification targets it", async () => {
      // A deleted reply renders nothing (#8296 keeps its row only so its own
      // replies keep a parent), so the id the notification carries has no
      // anchor left. The landing rule's matrix lives in
      // packages/core/issues/comment-deletion.test.ts.
      const root = { ...mockTimeline[0]!, id: "thread-root", parent_id: null };
      const previous = { ...mockTimeline[1]!, id: "reply-before", parent_id: root.id,
        content: "Still here", created_at: "2026-01-18T00:00:00Z" };
      const target = { ...mockTimeline[1]!, id: "deleted-reply", parent_id: root.id,
        content: "", deleted_at: "2026-01-19T00:00:00Z", created_at: "2026-01-19T00:00:00Z" };
      const kept = { ...mockTimeline[1]!, id: "reply-under-deleted", parent_id: target.id,
        content: "Kept below it", created_at: "2026-01-20T00:00:00Z" };
      mockApiObj.listTimeline.mockResolvedValue([root, previous, target, kept]);
      mockApiObj.listTasksByIssue.mockResolvedValue([]);
      renderIssueDetailWithHighlight(target.id);

      await waitFor(() => expect(
        hasHighlightedCommentBackground(document.getElementById(`comment-${previous.id}`)),
      ).toBe(true));
      // The tombstone itself never renders, so nothing waits on its anchor.
      expect(document.getElementById(`comment-${target.id}`)).toBeNull();
    });

    it("scrolls to the highlighted comment after both issue and timeline finish loading", async () => {
      renderIssueDetailWithHighlight("comment-2");

      // Wait for the comment row to mount. With initialItemCount in
      // production, items[0..targetIdx] are force-mounted on first commit;
      // the mock unconditionally inline-renders every item, so this just
      // waits for the regular render pass.
      await waitFor(() => {
        expect(
          document.getElementById("comment-comment-2"),
        ).not.toBeNull();
      });

      // The deep-link effect lands on AND highlights the target comment: it
      // drives the timeline container's scrollTop directly (jsdom has no
      // layout, so the scroll itself isn't observable here) and applies the
      // brand highlight background. Assert the user-facing highlight.
      await waitFor(() => {
        expect(
          hasHighlightedCommentBackground(document.getElementById("comment-comment-2")),
        ).toBe(true);
      });
    });

    it("highlights only the target root comment, not the whole thread", async () => {
      mockApiObj.listTimeline.mockResolvedValue([
        {
          type: "comment",
          id: "comment-root",
          actor_type: "member",
          actor_id: "user-1",
          content: "Root target",
          parent_id: null,
          created_at: "2026-01-18T00:00:00Z",
          updated_at: "2026-01-18T00:00:00Z",
          comment_type: "comment",
        } as TimelineEntry,
        {
          type: "comment",
          id: "reply-under-root",
          actor_type: "member",
          actor_id: "user-1",
          content: "Reply should stay neutral",
          parent_id: "comment-root",
          created_at: "2026-01-18T01:00:00Z",
          updated_at: "2026-01-18T01:00:00Z",
          comment_type: "comment",
        } as TimelineEntry,
      ]);

      renderIssueDetailWithHighlight("comment-root");

      await waitFor(() => {
        expect(document.getElementById("comment-comment-root")).not.toBeNull();
      });
      await waitFor(() => {
        expect(
          hasHighlightedCommentBackground(document.getElementById("comment-comment-root")),
        ).toBe(true);
      });

      const reply = document.getElementById("comment-reply-under-root");
      expect(reply).not.toBeNull();
      expect(hasHighlightedCommentBackground(reply)).toBe(false);
    });

    it("still scrolls when the timeline is ready before the issue (regression for inbox click)", async () => {
      // Reproduces the inbox-click race: timeline data is in the cache
      // before the issue resolves. While loading is true, IssueDetail
      // renders the loading skeleton (the timeline never mounts), so no
      // scroll/highlight can fire. After the issue resolves, the timeline
      // mounts and the deep-link effect lands on + highlights the comment.
      let resolveIssue: (value: Issue) => void = () => {};
      const issuePromise = new Promise<Issue>((resolve) => {
        resolveIssue = resolve;
      });
      mockApiObj.getIssue.mockReturnValue(issuePromise);

      renderIssueDetailWithHighlight("comment-2", "issue-1", { seedTimeline: true });

      expect(
        document.getElementById("comment-comment-2"),
      ).toBeNull();
      // Nothing highlighted while the loading skeleton is up.
      expect(hasHighlightedCommentBackground(document)).toBe(false);

      resolveIssue(mockIssue);

      await waitFor(() => {
        expect(
          document.getElementById("comment-comment-2"),
        ).not.toBeNull();
      });
      await waitFor(() => {
        expect(
          hasHighlightedCommentBackground(document.getElementById("comment-comment-2")),
        ).toBe(true);
      });
    });

    it("auto-expands a folded resolved thread when deep-link target is a reply inside it", async () => {
      // Seed a timeline where comment-3 is resolved (so it renders as a
      // resolved-bar by default) and has a reply, reply-1, whose id is the
      // deep-link target. The reply is not in the flat items array — only
      // the resolved-bar root is. The effect must detect this, expand the
      // thread, then on re-run scroll to the reply's id="comment-reply-1" node.
      const timelineWithResolvedThread: TimelineEntry[] = [
        ...mockTimeline,
        {
          type: "comment",
          id: "comment-3",
          actor_type: "member",
          actor_id: "user-1",
          content: "Resolved root",
          parent_id: null,
          created_at: "2026-01-18T00:00:00Z",
          updated_at: "2026-01-18T00:00:00Z",
          comment_type: "comment",
          resolved_at: "2026-01-19T00:00:00Z",
        } as TimelineEntry,
        {
          type: "comment",
          id: "reply-1",
          actor_type: "member",
          actor_id: "user-1",
          content: "Reply inside resolved thread",
          parent_id: "comment-3",
          created_at: "2026-01-18T01:00:00Z",
          updated_at: "2026-01-18T01:00:00Z",
          comment_type: "comment",
        } as TimelineEntry,
      ];
      mockApiObj.listTimeline.mockResolvedValue(timelineWithResolvedThread);

      const queryClient = createTestQueryClient();
      render(
        <I18nProvider locale="en" resources={TEST_RESOURCES}>
          <QueryClientProvider client={queryClient}>
            <IssueDetail issueId="issue-1" highlightCommentId="reply-1" />
          </QueryClientProvider>
        </I18nProvider>,
      );

      // After expansion, the reply must appear in the DOM (inside the now
      // -unfolded CommentCard) and the deep-link effect must land on + highlight
      // it. The reply highlight renders as a computed bg tint on its row (see
      // CommentCard's reply branch), so assert the row carries the brand tint.
      await waitFor(() => {
        expect(
          document.getElementById("comment-reply-1"),
        ).not.toBeNull();
      });
      await waitFor(() => {
        expect(
          document.getElementById("comment-reply-1")?.className,
        ).toContain("bg-[color-mix(in_srgb,var(--card)_95%,var(--brand)_5%)]");
      });
    });

    // CHE-436 acceptance criterion 4: replaying a notification for a reply
    // deep inside a long thread (r2 of 10, well outside the default
    // latest-three compact window) must land exactly on r2, not on the root
    // or on a neighboring reply — proof the target-root latch (01-04) and
    // the committed-generation gate (CHE-436) actually resolve to the right
    // DOM node once the thread force-opens.
    it("lands exactly on r2 of a 10-reply thread, not the root or a neighboring reply", async () => {
      const root = { ...mockTimeline[0]!, id: "ten-reply-root", content: "Ten-reply root" };
      const replies: TimelineEntry[] = Array.from({ length: 10 }, (_, i) => ({
        ...mockTimeline[1]!,
        id: `r${i + 1}`,
        parent_id: root.id,
        content: `Reply r${i + 1}`,
        created_at: `2026-01-16T00:${String(i).padStart(2, "0")}:00Z`,
      }));
      mockApiObj.listTimeline.mockResolvedValue([root, ...replies]);

      renderIssueDetailWithHighlight("r2", "issue-1");

      await waitFor(() => expect(document.getElementById("comment-r2")).not.toBeNull());
      await waitFor(() =>
        expect(hasHighlightedCommentBackground(document.getElementById("comment-r2"))).toBe(true),
      );
      // Not a neighboring reply (sibling row, so `hasHighlightedCommentBackground`'s
      // descendant walk is a clean, non-nested check here).
      expect(hasHighlightedCommentBackground(document.getElementById("comment-r1"))).toBe(false);
      expect(hasHighlightedCommentBackground(document.getElementById("comment-r3"))).toBe(false);
      // Not the root ITSELF (its own tint class, not r2's — the root wrapper
      // contains r2 as a descendant, so the recursive helper would always
      // read true here regardless of which row actually got the tint).
      const rootEl = document.getElementById(`comment-${root.id}`);
      expect(rootEl?.className ?? "").not.toContain(highlightedCommentBackgroundClass);
    });

    // Sol's CHE-436 PR #43 review, blocking finding 3: the landing effect's
    // own re-render (triggered by its OWN `setHighlightedId` call, since
    // `highlightedId` is component state) must not tear down its own
    // just-scheduled fade timeout/centering rAF. Before the fix,
    // `disclosureReveal.isCommitted` was a fresh function on every render, so
    // it was a fresh value in this effect's dependency array on every render
    // — including the one this same effect's `setHighlightedId` call causes —
    // which re-ran the effect's cleanup (cancelling the fade timer) the
    // instant after it started, then bailed out of a fresh landing because
    // `didHighlightRef` was already set. The visible symptom: the highlight
    // never fades, because the timer that would clear it was cancelled
    // before it could fire.
    it("fades the landed highlight after its own state update triggers a rerender", async () => {
      renderIssueDetailWithHighlight("comment-2");

      await waitFor(() =>
        expect(hasHighlightedCommentBackground(document.getElementById("comment-comment-2"))).toBe(true),
      );

      // The fade timeout is 2500ms; poll well past it with real timers. If
      // the landing effect's cleanup fired prematurely (cancelling the
      // timeout), this never becomes false and the test times out.
      await waitFor(
        () => expect(hasHighlightedCommentBackground(document.getElementById("comment-comment-2"))).toBe(false),
        { timeout: 4000, interval: 100 },
      );
    }, 6000);

    // CHE-436 acceptance criterion 5: a missing/deleted target must never
    // report false success by falling back to highlighting the root (or any
    // other node) — the landing effect requires the exact target element to
    // exist in the DOM before it records anything.
    it("never reports false success on the root when the deep-link target comment no longer exists", async () => {
      const root = { ...mockTimeline[0]!, id: "missing-target-root", content: "Root stays here" };
      mockApiObj.listTimeline.mockResolvedValue([root]);

      renderIssueDetailWithHighlight("deleted-comment-id", "issue-1");

      await screen.findByText("Root stays here");
      // Give the landing effect every chance to (incorrectly) fire.
      await act(async () => {
        await new Promise((resolve) => setTimeout(resolve, 0));
      });
      expect(hasHighlightedCommentBackground(document.getElementById(`comment-${root.id}`))).toBe(false);
      expect(document.getElementById("comment-deleted-comment-id")).toBeNull();
    });

    // Sol's CHE-436 PR #43 review, blocking finding 1: a consumed memento
    // (this exact target already landed once, in an earlier mount — e.g. a
    // tab switch back) must still reveal the target's fold, even though it
    // correctly skips the scroll/highlight/re-write. 01-DESIGN.md:76 —
    // "Memento restoration may suppress scrolling but must not suppress
    // revealing the target." Before the fix, the memento-consumed branch
    // returned before ever reaching the resolved-thread auto-expand logic,
    // so a target inside a still-collapsed resolved thread stayed hidden
    // forever on a memento-restored mount.
    it("still reveals a resolved-thread target on mount even when its memento was already consumed", async () => {
      const timelineWithResolvedThread: TimelineEntry[] = [
        ...mockTimeline,
        {
          type: "comment",
          id: "comment-3",
          actor_type: "member",
          actor_id: "user-1",
          content: "Resolved root",
          parent_id: null,
          created_at: "2026-01-18T00:00:00Z",
          updated_at: "2026-01-18T00:00:00Z",
          comment_type: "comment",
          resolved_at: "2026-01-19T00:00:00Z",
        } as TimelineEntry,
        {
          type: "comment",
          id: "reply-1",
          actor_type: "member",
          actor_id: "user-1",
          content: "Reply inside resolved thread",
          parent_id: "comment-3",
          created_at: "2026-01-18T01:00:00Z",
          updated_at: "2026-01-18T01:00:00Z",
          comment_type: "comment",
        } as TimelineEntry,
      ];
      mockApiObj.listTimeline.mockResolvedValue(timelineWithResolvedThread);

      // Fake adapter reporting the memento as already consumed for this exact
      // target — the scenario a real remount-after-tab-switch produces.
      const adapter: ScrollRestorationAdapter = {
        get: () => undefined,
        getViewState: (key) => (key === "highlight:issue-1" ? "reply-1" : undefined),
        setViewState: vi.fn(),
      };

      const queryClient = createTestQueryClient();
      render(
        <I18nProvider locale="en" resources={TEST_RESOURCES}>
          <QueryClientProvider client={queryClient}>
            <ScrollRestorationProvider adapter={adapter}>
              <IssueDetail issueId="issue-1" highlightCommentId="reply-1" />
            </ScrollRestorationProvider>
          </QueryClientProvider>
        </I18nProvider>,
      );

      // The thread must still auto-expand and reveal the reply, exactly like
      // a fresh (non-memento) landing — the memento only suppresses the
      // scroll/highlight/re-write that follows, never the reveal itself.
      await waitFor(() => {
        expect(document.getElementById("comment-reply-1")).not.toBeNull();
      });
      // The memento was already consumed for this target, so the effect must
      // not write it again.
      expect(adapter.setViewState).not.toHaveBeenCalled();
    });
  });

  it("marks a reply-resolved thread as resolved on the quick-jump rail", async () => {
    // A resolution on a REPLY leaves the thread expanded, so it flattens to a
    // plain `comment` item, not a `resolved-bar`. The rail must still read it
    // as resolved — proof the flag comes from deriveThreadResolution and not
    // from the fold state.
    mockApiObj.listTimeline.mockResolvedValue([
      ...mockTimeline,
      {
        type: "comment",
        id: "reply-1",
        actor_type: "member",
        actor_id: "user-1",
        content: "That fixed it",
        parent_id: "comment-1",
        created_at: "2026-01-18T00:00:00Z",
        updated_at: "2026-01-18T00:00:00Z",
        comment_type: "comment",
        resolved_at: "2026-01-19T00:00:00Z",
      } as TimelineEntry,
    ]);

    renderIssueDetail();

    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: "Started working on this (resolved)" }),
      ).toBeInTheDocument();
    });
    expect(screen.getByRole("button", { name: "I can help with this" })).toBeInTheDocument();
  });

  it("sends empty description when editor is cleared", async () => {
    renderIssueDetail();

    const editor = await screen.findByDisplayValue("Add JWT auth to the backend");
    fireEvent.change(editor, { target: { value: "" } });

    await waitFor(() => {
      expect(mockApiObj.updateIssue).toHaveBeenCalledWith(
        "issue-1",
        expect.objectContaining({
          description: "",
          description_base: "Add JWT auth to the backend",
        }),
      );
    });
  });

  // Descriptions are last-write-wins (MUL-6971). The baseline still ships as
  // channel-media merge metadata, but a rejected save no longer opens a compare
  // panel — that panel fired on the editor's own autosave and wedged the
  // session, and the title/comment editors keep the compare flow they can
  // actually satisfy.
  it("keeps editing after a failed description save without a compare panel", async () => {
    mockApiObj.updateIssue.mockRejectedValueOnce({
      body: { code: "revision_conflict" },
    });
    renderIssueDetail();

    const editor = await screen.findByDisplayValue("Add JWT auth to the backend");
    fireEvent.focus(editor);
    fireEvent.change(editor, { target: { value: "My local description" } });

    await waitFor(() =>
      expect(mockApiObj.updateIssue).toHaveBeenCalledWith(
        "issue-1",
        expect.objectContaining({
          description: "My local description",
          description_base: "Add JWT auth to the backend",
        }),
      ),
    );
    // The compare panel's own actions — still rendered for a title conflict,
    // so their absence is about the description, not a missing translation.
    expect(
      screen.queryByRole("button", { name: "Keep my version" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Use the latest version" }),
    ).not.toBeInTheDocument();
    expect(screen.getByDisplayValue("My local description")).toBeVisible();

    // The save gate reopened: the next edit still reaches the server.
    fireEvent.change(editor, { target: { value: "My next description" } });
    await waitFor(() =>
      expect(mockApiObj.updateIssue).toHaveBeenLastCalledWith(
        "issue-1",
        expect.objectContaining({ description: "My next description" }),
      ),
    );
  });

  it("serializes description saves and rebases the queued draft on submitted content", async () => {
    let resolveFirst!: (issue: Issue) => void;
    const firstSave = new Promise<Issue>((resolve) => {
      resolveFirst = resolve;
    });
    mockApiObj.updateIssue
      .mockReturnValueOnce(firstSave)
      .mockResolvedValueOnce({
        ...mockIssue,
        description: "Second local description",
        revision: 5,
      });
    renderIssueDetail();

    const editor = await screen.findByDisplayValue("Add JWT auth to the backend");
    fireEvent.focus(editor);
    fireEvent.change(editor, { target: { value: "First local description" } });
    await waitFor(() => expect(mockApiObj.updateIssue).toHaveBeenCalledTimes(1));
    fireEvent.change(editor, { target: { value: "Second local description" } });
    expect(mockApiObj.updateIssue).toHaveBeenCalledTimes(1);

    await act(async () => {
      resolveFirst({ ...mockIssue, description: "First local description", revision: 4 });
      await firstSave;
    });

    await waitFor(() => expect(mockApiObj.updateIssue).toHaveBeenCalledTimes(2));
    expect(mockApiObj.updateIssue).toHaveBeenNthCalledWith(
      2,
      "issue-1",
      expect.objectContaining({
        description: "Second local description",
        description_base: "First local description",
      }),
    );
  });

  it("ignores a late description callback after switching issues", async () => {
    const queryClient = createTestQueryClient();
    const issue2: Issue = {
      ...mockIssue,
      id: "issue-2",
      identifier: "TES-2",
      description: "Second issue description",
      revision: 8,
    };
    queryClient.setQueryData(["issues", "ws-1", "detail", "issue-2"], issue2);
    mockApiObj.getIssue.mockImplementation((issueId: string) =>
      Promise.resolve(issueId === "issue-2" ? issue2 : mockIssue),
    );

    let resolveFirst!: (issue: Issue) => void;
    const firstSave = new Promise<Issue>((resolve) => {
      resolveFirst = resolve;
    });
    mockApiObj.updateIssue
      .mockReturnValueOnce(firstSave)
      .mockResolvedValueOnce({ ...issue2, description: "Issue two draft", revision: 9 });

    const ui = (issueId: string) => (
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={queryClient}>
          <IssueDetail issueId={issueId} />
        </QueryClientProvider>
      </I18nProvider>
    );
    const { rerender } = render(ui("issue-1"));

    const issueOneEditor = await screen.findByDisplayValue("Add JWT auth to the backend");
    fireEvent.focus(issueOneEditor);
    fireEvent.change(issueOneEditor, {
      target: { value: "Issue one draft" },
    });
    await waitFor(() => expect(mockApiObj.updateIssue).toHaveBeenCalledTimes(1));

    rerender(ui("issue-2"));
    const issueTwoEditor = await screen.findByDisplayValue("Second issue description");
    fireEvent.focus(issueTwoEditor);
    fireEvent.change(issueTwoEditor, {
      target: { value: "Issue two draft" },
    });
    await waitFor(() => expect(mockApiObj.updateIssue).toHaveBeenCalledTimes(2));
    expect(mockApiObj.updateIssue).toHaveBeenNthCalledWith(
      2,
      "issue-2",
      expect.objectContaining({ description_base: "Second issue description" }),
    );

    await act(async () => {
      resolveFirst({ ...mockIssue, description: "Issue one draft", revision: 4 });
      await firstSave;
    });

    expect(mockApiObj.updateIssue).toHaveBeenCalledTimes(2);
    expect(screen.getByDisplayValue("Issue two draft")).toBeVisible();
  });

  it("expands a collapsed description before a dropped file inserts", async () => {
    descriptionMeasurement.current = {
      totalRows: 13,
      hiddenRows: 1,
      hasOverflow: true,
      lineHeight: 20,
      previewText: "Add JWT auth to the backend",
    };
    mockUploadWithToast.mockResolvedValue(undefined);
    renderIssueDetail();

    await screen.findByDisplayValue("Add JWT auth to the backend");
    const editor = document.querySelector("[data-description-editor]")!;
    expect(editor).toHaveAttribute("aria-hidden", "true");
    expect(descDropZoneOnDrop.current).toBeTruthy();

    // Assert the ordering at the moment uploadFile is actually invoked, not
    // after `act` flushes: the expansion must have already committed to the
    // DOM (no aria-hidden/inert) by the time this fires.
    let editorWasInertAtInsert: boolean | null = null;
    descEditorUploadFile.mockImplementationOnce(() => {
      editorWasInertAtInsert = editor.hasAttribute("aria-hidden") || editor.hasAttribute("inert");
    });

    const droppedFile = new File(["x"], "dropped.png", { type: "image/png" });
    await act(async () => {
      descDropZoneOnDrop.current!([droppedFile]);
    });

    expect(editorWasInertAtInsert).toBe(false);
    expect(editor).not.toHaveAttribute("aria-hidden");
    expect(descEditorUploadFile).toHaveBeenCalledWith(droppedFile);
  });

  it("expands a collapsed description before an Attach-file selection inserts", async () => {
    descriptionMeasurement.current = {
      totalRows: 13,
      hiddenRows: 1,
      hasOverflow: true,
      lineHeight: 20,
      previewText: "Add JWT auth to the backend",
    };
    mockUploadWithToast.mockResolvedValue(undefined);
    renderIssueDetail();

    await screen.findByDisplayValue("Add JWT auth to the backend");
    const editor = document.querySelector("[data-description-editor]")!;
    expect(editor).toHaveAttribute("aria-hidden", "true");

    let editorWasInertAtInsert: boolean | null = null;
    descEditorUploadFile.mockImplementationOnce(() => {
      editorWasInertAtInsert = editor.hasAttribute("aria-hidden") || editor.hasAttribute("inert");
    });

    const selectedFile = new File(["x"], "selected.png", { type: "image/png" });
    const fileInput = descriptionFileInput();
    expect(fileInput).toBeTruthy();
    await act(async () => {
      Object.defineProperty(fileInput, "files", { value: [selectedFile], configurable: true });
      fireEvent.change(fileInput);
    });

    expect(editorWasInertAtInsert).toBe(false);
    expect(editor).not.toHaveAttribute("aria-hidden");
    expect(descEditorUploadFile).toHaveBeenCalledWith(selectedFile);
  });

  it("keeps focus on Show less after expanding, instead of dropping it via collapseDisabled", async () => {
    // Regression: `onFocusCapture` on the wrapper around DescriptionDisclosure
    // fired for ANY focus inside it, including the Show more/less button
    // itself. Focusing "Show more" then expanding flipped `descriptionFocused`
    // true from that same focus event; on the next render the button (now
    // "Show less") read `disabled={expanded && collapseDisabled}` and became
    // disabled while the browser's focus was still on it — a disabled element
    // cannot hold focus, so focus silently dropped to <body>. The guard must
    // only count focus landing inside the actual editor as "editing."
    descriptionMeasurement.current = {
      totalRows: 13,
      hiddenRows: 1,
      hasOverflow: true,
      lineHeight: 20,
      previewText: "Add JWT auth to the backend",
    };
    renderIssueDetail();

    await screen.findByDisplayValue("Add JWT auth to the backend");
    const showMore = screen.getByRole("button", { name: /Show more/ });
    act(() => showMore.focus());
    fireEvent.keyDown(showMore, { key: "Enter" });
    fireEvent.click(showMore);

    const showLess = await screen.findByRole("button", { name: "Show less" });
    expect(showLess).not.toBeDisabled();
    expect(document.activeElement).toBe(showLess);
  });

  it("re-enables Show less once a pending upload's attachment ids are bound", async () => {
    descriptionMeasurement.current = {
      totalRows: 13,
      hiddenRows: 1,
      hasOverflow: true,
      lineHeight: 20,
      previewText: "Add JWT auth to the backend",
    };
    const attachment: Attachment = {
      id: "attach-1",
      workspace_id: "ws-1",
      issue_id: "issue-1",
      comment_id: null,
      chat_session_id: null,
      chat_message_id: null,
      uploader_type: "member",
      uploader_id: "user-1",
      filename: "pasted.png",
      url: "https://files.example.com/attach-1",
      download_url: "https://files.example.com/attach-1",
      markdown_url: "/api/attachments/attach-1/download",
      content_type: "image/png",
      size_bytes: 100,
      created_at: "2026-01-20T00:00:00Z",
    };
    mockUploadWithToast.mockResolvedValue(attachment);
    mockApiObj.updateIssue.mockResolvedValue({
      ...mockIssue,
      description: "Add JWT auth to the backend ![pasted](/api/attachments/attach-1/download)",
      revision: 4,
    });
    renderIssueDetail();

    await screen.findByDisplayValue("Add JWT auth to the backend");
    fireEvent.click(screen.getByRole("button", { name: /Show more/ }));
    await screen.findByRole("button", { name: "Show less" });

    // handleDescriptionUpload is passed as ContentEditor's onUploadFile prop;
    // the real editor invokes it internally on paste/drop/insert. The mocked
    // editor's imperative uploadFile() has no document to insert into, so
    // this is the one path that actually exercises
    // handleDescriptionUpload → uploadWithToast → descPendingAttachments.
    expect(descOnUploadFile.current).toBeTruthy();
    const file = new File(["x"], "pasted.png", { type: "image/png" });
    await act(async () => {
      await descOnUploadFile.current!(file);
    });
    expect(mockUploadWithToast).toHaveBeenCalledWith(file);

    const showLess = await screen.findByRole("button", { name: "Show less" });
    expect(showLess).toBeDisabled();

    // Reference the pasted attachment from the markdown so it binds on save.
    const editor = screen.getByTestId("rich-text-editor");
    fireEvent.change(editor, {
      target: { value: "Add JWT auth to the backend ![pasted](/api/attachments/attach-1/download)" },
    });
    await waitFor(() =>
      expect(mockApiObj.updateIssue).toHaveBeenCalledWith(
        "issue-1",
        expect.objectContaining({ attachment_ids: ["attach-1"] }),
      ),
    );

    await waitFor(() => expect(screen.getByRole("button", { name: "Show less" })).not.toBeDisabled());
  });

  it("keeps a title draft visible when its captured content conflicts", async () => {
    mockApiObj.updateIssue.mockRejectedValueOnce({
      body: { code: "revision_conflict" },
    });
    renderIssueDetail();

    fireEvent.click(await screen.findByRole("button", { name: "Implement authentication" }));
    const editor = await screen.findByTestId("title-editor");
    fireEvent.change(editor, { target: { value: "My local title" } });
    fireEvent.blur(editor);

    await waitFor(() =>
      expect(mockApiObj.updateIssue).toHaveBeenCalledWith(
        "issue-1",
        expect.objectContaining({
          title: "My local title",
          title_base: "Implement authentication",
        }),
      ),
    );
    expect(
      await screen.findByText("The title was changed concurrently. Compare both versions."),
    ).toBeVisible();
    expect(screen.getAllByText("My local title").length).toBeGreaterThan(0);
    expect(screen.getByDisplayValue("My local title")).toBeVisible();
  });

  it("restores the server title when the user takes the latest version", async () => {
    mockApiObj.updateIssue.mockRejectedValueOnce({
      body: { code: "revision_conflict" },
    });
    renderIssueDetail();

    fireEvent.click(await screen.findByRole("button", { name: "Implement authentication" }));
    const editor = await screen.findByTestId("title-editor");
    fireEvent.change(editor, { target: { value: "My local title" } });
    fireEvent.blur(editor);

    expect(
      await screen.findByText("The title was changed concurrently. Compare both versions."),
    ).toBeVisible();
    const callsBeforeDiscard = mockApiObj.updateIssue.mock.calls.length;

    fireEvent.click(screen.getByRole("button", { name: "Use the latest version" }));

    await waitFor(() =>
      expect(
        screen.queryByText("The title was changed concurrently. Compare both versions."),
      ).not.toBeInTheDocument(),
    );
    // TitleEditor takes its text at mount, so the remount is what puts the
    // server title back — see titleResetToken.
    expect(await screen.findByDisplayValue("Implement authentication")).toBeVisible();
    // Taking the server version is local-only: the server already holds it.
    expect(mockApiObj.updateIssue).toHaveBeenCalledTimes(callsBeforeDiscard);
  });

  describe("sub-issues list", () => {
    beforeEach(() => {
      useSubIssueDisplayStore.setState({
        rowProperties: { ...DEFAULT_SUB_ISSUE_ROW_PROPERTIES },
        rowPropertyIds: [],
      });
    });

    const label = (id: string, name: string): Label => ({
      id,
      workspace_id: "ws-1",
      resource_type: "issue",
      name,
      color: "#3b82f6",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    });

    const subIssue = (overrides: Partial<Issue>): Issue => ({
      ...mockIssue,
      parent_issue_id: "issue-1",
      assignee_type: null,
      assignee_id: null,
      due_date: null,
      priority: "none",
      ...overrides,
    });

    it("renders priority, labels, due date and nested progress on rows", async () => {
      mockApiObj.listChildIssues.mockResolvedValue({
        issues: [
          subIssue({
            id: "child-1",
            number: 11,
            identifier: "TES-11",
            title: "Fix login flow",
            priority: "urgent",
            labels: [label("l1", "backend"), label("l2", "auth")],
            // Past date-only value → overdue styling on an open issue.
            due_date: "2020-01-01",
          }),
          subIssue({
            id: "child-2",
            number: 12,
            identifier: "TES-12",
            title: "Ship dashboards",
          }),
        ],
      });
      mockApiObj.getChildIssueProgress.mockResolvedValue({
        progress: [{ parent_issue_id: "child-1", done: 1, total: 3 }],
      });

      renderIssueDetail();

      await screen.findByText("Fix login flow");
      // Label chips ride inside the row link.
      expect(screen.getByText("backend")).toBeInTheDocument();
      expect(screen.getByText("auth")).toBeInTheDocument();
      // Nested own-children progress for child-1 only (header shows 0/2).
      expect(await screen.findByText("1/3")).toBeInTheDocument();
      // Overdue open issue renders its due date in the destructive tone.
      const due = screen.getByText("Jan 1");
      expect(due.closest("span")?.className).toContain("text-destructive");
      // Bare row shows no due date / no progress chip of its own.
      const bareRow = screen.getByText("Ship dashboards").closest("a");
      expect(bareRow?.textContent).not.toContain("/");
    });

    it("hides fields the user toggled off in the display preference", async () => {
      useSubIssueDisplayStore.setState({
        rowProperties: {
          ...DEFAULT_SUB_ISSUE_ROW_PROPERTIES,
          labels: false,
          dueDate: false,
        },
      });
      mockApiObj.listChildIssues.mockResolvedValue({
        issues: [
          subIssue({
            id: "child-1",
            number: 11,
            identifier: "TES-11",
            title: "Fix login flow",
            labels: [label("l1", "backend")],
            due_date: "2020-01-01",
          }),
        ],
      });

      renderIssueDetail();

      await screen.findByText("Fix login flow");
      expect(screen.queryByText("backend")).not.toBeInTheDocument();
      expect(screen.queryByText("Jan 1")).not.toBeInTheDocument();
    });

    it("renders opted-in custom property chips on rows that carry a value", async () => {
      const estimate = {
        id: "prop-1",
        workspace_id: "ws-1",
        name: "Estimate",
        type: "text",
        config: {},
        position: 0,
        archived: false,
        created_at: "2026-01-01T00:00:00Z",
        updated_at: "2026-01-01T00:00:00Z",
      };
      mockApiObj.listProperties.mockResolvedValue({
        properties: [estimate],
        total: 1,
      });
      useSubIssueDisplayStore.setState({ rowPropertyIds: ["prop-1"] });
      mockApiObj.listChildIssues.mockResolvedValue({
        issues: [
          subIssue({
            id: "child-1",
            number: 11,
            identifier: "TES-11",
            title: "Fix login flow",
            properties: { "prop-1": "Sprint 3" },
          }),
          subIssue({
            id: "child-2",
            number: 12,
            identifier: "TES-12",
            title: "Ship dashboards",
          }),
        ],
      });

      renderIssueDetail();

      await screen.findByText("Fix login flow");
      // Chip renders only on the row that has a value for the property.
      expect(await screen.findByText("Sprint 3")).toBeInTheDocument();
      expect(screen.getAllByText("Sprint 3")).toHaveLength(1);
    });

    it("mutes the due date on done sub-issues even when past", async () => {
      mockApiObj.listChildIssues.mockResolvedValue({
        issues: [
          subIssue({
            id: "child-1",
            number: 11,
            identifier: "TES-11",
            title: "Wrapped up",
            status: "done",
            due_date: "2020-01-01",
          }),
        ],
      });

      renderIssueDetail();

      await screen.findByText("Wrapped up");
      const due = screen.getByText("Jan 1");
      expect(due.closest("span")?.className).not.toContain("text-destructive");
      expect(due.closest("span")?.className).toContain("text-muted-foreground");
    });
  });

  // Deliberately drives the real Base UI DropdownMenu rather than a stub: the
  // bug these tests pin (MUL-5710) was a handler wired to `onSelect`, which
  // typechecks because Menu.Item's props extend the whole div attribute set,
  // then lands on the DOM as the native text-selection event and never fires.
  // Only the real menu reproduces that; any hand-rolled mock hides it.
  describe("unsubscribe menu", () => {
    const subscribedAsMember = [
      {
        issue_id: "issue-1",
        user_type: "member" as const,
        user_id: "user-1",
        reason: "manual" as const,
        created_at: "2026-01-01T00:00:00Z",
      },
    ];

    beforeEach(() => {
      mockApiObj.listIssueSubscribers.mockResolvedValue(subscribedAsMember);
      // The menu only exists when there is a sub-tree for its second item to
      // act on. A childless issue renders a direct button instead — covered
      // by the "no sub-issues" tests below (MUL-5714).
      mockApiObj.listChildIssues.mockResolvedValue({
        issues: [{ ...mockIssue, id: "child-1", parent_issue_id: "issue-1" }],
      });
    });

    // Base UI portals the popup onto document.body; RTL unmounts it, but wipe
    // the body too so a leftover portal can't duplicate menu item names.
    afterEach(() => {
      document.body.innerHTML = "";
    });

    async function openUnsubscribeMenu() {
      renderIssueDetail();
      fireEvent.click(await screen.findByText("Unsubscribe"));
      return screen.findByRole("menu");
    }

    it("unsubscribes the current member when the single-issue item is clicked", async () => {
      await openUnsubscribeMenu();

      fireEvent.click(
        screen.getByRole("menuitem", { name: "Unsubscribe from this issue" }),
      );

      await waitFor(() =>
        expect(mockApiObj.unsubscribeFromIssue).toHaveBeenCalledWith(
          "issue-1",
          "user-1",
          "member",
        ),
      );
      expect(mockApiObj.unsubscribeFromIssueSubtree).not.toHaveBeenCalled();
    });

    it("unsubscribes from the whole subtree when the subtree item is clicked", async () => {
      await openUnsubscribeMenu();

      fireEvent.click(
        screen.getByRole("menuitem", {
          name: "Unsubscribe from this issue and its sub-issues",
        }),
      );

      await waitFor(() =>
        expect(mockApiObj.unsubscribeFromIssueSubtree).toHaveBeenCalledWith(
          "issue-1",
          "user-1",
          "member",
        ),
      );
      expect(mockApiObj.unsubscribeFromIssue).not.toHaveBeenCalled();
    });

    it("still offers the menu while the child count is unknown", async () => {
      // Children never resolve, so the component cannot yet tell a childless
      // issue from one with a sub-tree. The menu is the safe answer: unlike
      // the direct button it never picks an opt-out scope for the user.
      mockApiObj.listChildIssues.mockReturnValue(new Promise(() => {}));
      renderIssueDetail();

      const control = await screen.findByText("Unsubscribe");

      expect(control.getAttribute("aria-haspopup")).toBe("menu");
    });
  });

  // The reported bug: on an issue with no sub-issues the only way to leave was
  // a menu whose second item pointed at a sub-tree that does not exist
  // (MUL-5714).
  describe("unsubscribe without sub-issues", () => {
    const subscribedAsMember = [
      {
        issue_id: "issue-1",
        user_type: "member" as const,
        user_id: "user-1",
        reason: "manual" as const,
        created_at: "2026-01-01T00:00:00Z",
      },
    ];

    beforeEach(() => {
      mockApiObj.listIssueSubscribers.mockResolvedValue(subscribedAsMember);
      mockApiObj.listChildIssues.mockResolvedValue({ issues: [] });
    });

    afterEach(() => {
      document.body.innerHTML = "";
    });

    // The label is the same either way, so aria-haspopup is what separates the
    // menu trigger from the plain button. The subscribers query resolves first,
    // so the control is briefly the trigger before the child count settles —
    // wait for the collapsed form rather than grabbing the first match.
    async function findDirectUnsubscribeButton() {
      return waitFor(() => {
        const el = screen.getByText("Unsubscribe");
        expect(el.getAttribute("aria-haspopup")).toBeNull();
        return el;
      });
    }

    it("unsubscribes in one click, with no menu", async () => {
      renderIssueDetail();

      fireEvent.click(await findDirectUnsubscribeButton());

      await waitFor(() =>
        expect(mockApiObj.unsubscribeFromIssue).toHaveBeenCalledWith(
          "issue-1",
          "user-1",
          "member",
        ),
      );
      expect(screen.queryByRole("menu")).toBeNull();
    });

    // The scope choice, pinned. RemoveIssueSubscriber writes
    // opt_out_scope='issue'; the subtree route writes 'subtree', which also
    // blocks FUTURE children from re-subscribing the user. Collapsing the menu
    // must not quietly upgrade a one-issue opt-out into a whole-tree one
    // (server/pkg/db/queries/subscriber.sql).
    it("uses the root-only route, never the subtree one", async () => {
      renderIssueDetail();

      fireEvent.click(await findDirectUnsubscribeButton());

      await waitFor(() =>
        expect(mockApiObj.unsubscribeFromIssue).toHaveBeenCalled(),
      );
      expect(mockApiObj.unsubscribeFromIssueSubtree).not.toHaveBeenCalled();
    });

    it("sends one request for a rapid double-click", async () => {
      let release: (() => void) | undefined;
      mockApiObj.unsubscribeFromIssue.mockReturnValue(
        new Promise<void>((resolve) => {
          release = () => resolve();
        }),
      );
      renderIssueDetail();
      const button = await findDirectUnsubscribeButton();

      // Same tick, no await between. React Query flushes isPending in a
      // microtask, so `disabled` has not landed yet and the second click still
      // reaches an enabled button — the in-flight guard is what stops it. Two
      // overlapping toggles is the one case the mutation's whole-list snapshot
      // cannot survive: the second snapshots the first one's optimistic patch
      // and rolls back to it (MUL-5714).
      fireEvent.click(button);
      fireEvent.click(button);

      // Both handlers already ran synchronously; only the request dispatch is
      // async. So once any call has landed, the count is final.
      await waitFor(() =>
        expect(mockApiObj.unsubscribeFromIssue).toHaveBeenCalled(),
      );
      expect(mockApiObj.unsubscribeFromIssue).toHaveBeenCalledTimes(1);
      release?.();
    });

    it("disables the button while the toggle is in flight, then reports failure", async () => {
      let reject: ((err: Error) => void) | undefined;
      mockApiObj.unsubscribeFromIssue.mockReturnValue(
        new Promise<void>((_resolve, rej) => {
          reject = (err) => rej(err);
        }),
      );
      renderIssueDetail();
      const button = await findDirectUnsubscribeButton();

      fireEvent.click(button);

      await waitFor(() =>
        expect((button as HTMLButtonElement).disabled).toBe(true),
      );

      reject?.(new Error("boom"));

      // The optimistic patch rolls itself back, restoring the exact row the
      // user started from — without this message that is indistinguishable
      // from a button that never fired.
      await waitFor(() =>
        expect(toast.error).toHaveBeenCalledWith(
          enIssues.detail.subscription_update_failed,
        ),
      );
      await waitFor(() =>
        expect((button as HTMLButtonElement).disabled).toBe(false),
      );
    });
  });

  // Before the subscribers query resolves the hook's list defaults to empty,
  // which reads as "not subscribed" for everyone. Rendering that default
  // showed a Subscribe button to people who were already subscribed, and a
  // click landing in that window sent a subscribe instead of the unsubscribe
  // they meant (MUL-5714).
  describe("subscription state before the query resolves", () => {
    afterEach(() => {
      document.body.innerHTML = "";
    });

    it("renders no subscribe control while subscribers are loading", async () => {
      mockApiObj.listIssueSubscribers.mockReturnValue(new Promise(() => {}));
      renderIssueDetail();

      // Wait for the issue itself, so this asserts on a rendered page rather
      // than on the loading skeleton.
      await screen.findByText("Implement authentication");

      expect(screen.queryByText("Subscribe")).toBeNull();
      expect(screen.queryByText("Unsubscribe")).toBeNull();
    });

    it("renders Subscribe once the query says the user is not subscribed", async () => {
      mockApiObj.listIssueSubscribers.mockResolvedValue([]);
      renderIssueDetail();

      expect(await screen.findByText("Subscribe")).toBeTruthy();
    });
  });

  // Same cold-cache hazard as the Subscribe button, but worse to act on. Every
  // checkbox here is drawn from the subscribers list, so an unresolved query
  // renders everyone — including people who ARE subscribed — as unchecked.
  // Clicking one of those rows sends an explicit subscribe, which rewrites the
  // target's reason to 'manual' and clears any opt-out scope
  // (server/pkg/db/queries/subscriber.sql), discarding a delegated
  // subscription or a deliberate opt-out (MUL-5714).
  describe("subscriber picker before the query resolves", () => {
    // The picker sits next to the subscribe control in the Activity header.
    // Anchor on the heading, not on that control — the whole point of these
    // cases is that it is not rendered yet.
    async function openSubscriberPicker() {
      const heading = await screen.findByText("Activity");
      const header = heading.parentElement?.parentElement;
      const trigger = header?.querySelector('[data-slot="popover-trigger"]');
      if (!trigger) throw new Error("subscriber picker trigger not found");
      fireEvent.click(trigger);
      return waitFor(() => {
        const content = document.querySelector('[data-slot="popover-content"]');
        if (!content) throw new Error("picker did not open");
        return content;
      });
    }

    function memberRow(content: Element) {
      const row = Array.from(
        content.querySelectorAll('[data-slot="command-item"]'),
      ).find((el) => el.textContent?.includes("Test User"));
      if (!row) throw new Error("member row not found");
      return row;
    }

    it("disables the rows while subscribers are loading", async () => {
      mockApiObj.listIssueSubscribers.mockReturnValue(new Promise(() => {}));
      renderIssueDetail();

      const row = memberRow(await openSubscriberPicker());

      expect(row.getAttribute("data-disabled")).toBe("true");

      fireEvent.click(row);

      expect(mockApiObj.subscribeToIssue).not.toHaveBeenCalled();
      expect(mockApiObj.unsubscribeFromIssue).not.toHaveBeenCalled();
    });

    it("disables the rows when the subscribers query failed", async () => {
      mockApiObj.listIssueSubscribers.mockRejectedValue(new Error("boom"));
      renderIssueDetail();

      const row = memberRow(await openSubscriberPicker());

      expect(row.getAttribute("data-disabled")).toBe("true");

      fireEvent.click(row);

      expect(mockApiObj.subscribeToIssue).not.toHaveBeenCalled();
      expect(mockApiObj.unsubscribeFromIssue).not.toHaveBeenCalled();
    });

    it("enables the rows once the query has a real answer", async () => {
      mockApiObj.listIssueSubscribers.mockResolvedValue([]);
      renderIssueDetail();
      // Wait for the resolved state before opening, so this is not just the
      // pending case passing by accident.
      await screen.findByText("Subscribe");

      const row = memberRow(await openSubscriberPicker());

      expect(row.getAttribute("data-disabled")).not.toBe("true");

      fireEvent.click(row);

      await waitFor(() =>
        expect(mockApiObj.subscribeToIssue).toHaveBeenCalledWith(
          "issue-1",
          "user-1",
          "member",
        ),
      );
    });
  });

  // MUL-7211 regression: a standalone run's published reply belongs at the
  // reply's own time. It used to render in the run's ENQUEUE slot while the
  // card showed the reply time, pushing it above every comment written while
  // the run worked. Ordering matrix lives in comment-runs.test.ts.
  it("renders an assignment run's reply after the comments it followed", async () => {
    mockApiObj.listTimeline.mockResolvedValue([
      {
        type: "comment", id: "midway", actor_type: "member", actor_id: "user-1",
        content: "Remember the E2E pass", parent_id: null,
        created_at: "2026-01-17T00:00:00Z", updated_at: "2026-01-17T00:00:00Z", comment_type: "comment",
      },
      {
        type: "comment", id: "run-reply", actor_type: "agent", actor_id: "agent-1",
        content: "step1 done", parent_id: null, source_task_id: "task-early",
        created_at: "2026-01-18T00:00:00Z", updated_at: "2026-01-18T00:00:00Z", comment_type: "comment",
      },
    ]);
    mockApiObj.listTasksByIssue.mockResolvedValue([{
      id: "task-early", agent_id: "agent-1", runtime_id: "rt-1", issue_id: "issue-1",
      kind: "issue", status: "completed", priority: 0,
      dispatched_at: "2026-01-16T00:00:00Z", started_at: "2026-01-16T00:00:00Z",
      completed_at: "2026-01-18T00:00:00Z", result: { comment: "step1 done" }, error: null,
      created_at: "2026-01-16T00:00:00Z", delivered_comment_ids: [],
    }]);

    const { container } = renderIssueDetail();
    await screen.findByText("Remember the E2E pass");
    await screen.findByText("step1 done");

    const rendered = Array.from(container.querySelectorAll("[id^='comment-']")).map((el) => el.id);
    expect(rendered.indexOf("comment-midway")).toBeLessThan(rendered.indexOf("comment-run-reply"));
  });

});

describe("groupSubIssuesByStage", () => {
  const child = (id: string, stage: number | null): Issue => ({
    ...mockIssue,
    id,
    parent_issue_id: "parent-1",
    stage,
  });

  it("returns a single null-stage group when nothing is staged", () => {
    const groups = groupSubIssuesByStage([child("a", null), child("b", null)]);
    expect(groups).toHaveLength(1);
    expect(groups[0]?.stage).toBeNull();
    expect(groups[0]?.items.map((i) => i.id)).toEqual(["a", "b"]);
  });

  it("orders staged groups ascending with the unstaged group last", () => {
    const groups = groupSubIssuesByStage([
      child("s2", 2),
      child("u", null),
      child("s1a", 1),
      child("s1b", 1),
    ]);
    expect(groups.map((g) => g.stage)).toEqual([1, 2, null]);
    expect(groups[0]?.items.map((i) => i.id)).toEqual(["s1a", "s1b"]);
    expect(groups[2]?.items.map((i) => i.id)).toEqual(["u"]);
  });

  it("omits the unstaged group when every child is staged", () => {
    const groups = groupSubIssuesByStage([child("s1", 1), child("s2", 2)]);
    expect(groups.map((g) => g.stage)).toEqual([1, 2]);
  });
});
