/**
 * @vitest-environment jsdom
 *
 * A card dragged across a project board whose workflow omits a column. The
 * mirror follows the pointer through the allowed column, and the blocked column
 * after it leaves the mirror where it was. The drop must be judged on the
 * column the pointer is released over, not on the mirror, or the card lands in
 * a column the user never chose.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ViewStoreProvider } from "@multica/core/issues/stores/view-store-context";
import { getIssueSurfaceViewStore } from "@multica/core/issues/stores/surface-view-store";
import { setApiInstance } from "@multica/core/api";
import { projectListOptions } from "@multica/core/projects/queries";
import type { Issue, IssueWorkflow, Project } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { IssueContextMenuProvider } from "../actions/issue-actions-context-menu";
import { statusGroupId } from "../utils/drag-utils";
import { BoardView } from "./board-view";

const PREVIEW = {
  previous_assignee_type: null,
  previous_assignee_id: null,
  handler_type: "member",
  handler_id: "member-1",
};
vi.mock("../../workflows/handoff-confirm-dialog", () => ({
  WorkflowHandoffConfirmDialog: ({ issue, onConfirm }: { issue: unknown; onConfirm: (o: unknown) => void }) =>
    issue ? <button onClick={() => onConfirm({ stopPreviousRuns: true, preview: PREVIEW })}>confirm handoff</button> : null,
}));

const mockToastInfo = vi.hoisted(() => vi.fn());
const mockToastError = vi.hoisted(() => vi.fn());
vi.mock("sonner", () => ({ toast: { info: mockToastInfo, error: mockToastError } }));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));

const PROJECT_ID = "11111111-1111-4111-8111-111111111111";
const WORKFLOW: IssueWorkflow = {
  id: "wf-1",
  workspace_id: "ws-1",
  name: "Delivery",
  description: "",
  is_default: false,
  steps: [
    { status_key: "todo", handler: { type: "none" }, instructions: "" },
    { status_key: "in_progress", handler: { type: "none" }, instructions: "" },
    { status_key: "done", handler: { type: "none" }, instructions: "" },
  ],
  project_ids: [PROJECT_ID],
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
} as IssueWorkflow;
const workflowState = vi.hoisted(() => ({ workflows: [] as unknown[], loaded: true }));

vi.mock("@multica/core/issue-workflows", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/issue-workflows")>()),
  useIssueWorkflows: () => ({ workflows: workflowState.workflows, isLoaded: workflowState.loaded, isError: false, retry: () => {} }),
}));

vi.mock("@multica/core/properties", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/properties")>()),
  propertyListOptions: () => ({ queryKey: ["properties"], queryFn: async () => [] }),
  useSetIssueProperty: () => ({ mutate: () => {} }),
  useUnsetIssueProperty: () => ({ mutate: () => {} }),
}));

vi.mock("@multica/core/workspace/hooks", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/workspace/hooks")>()),
  useActorName: () => ({ getActorName: () => "Someone" }),
}));

vi.mock("@multica/core/auth", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/auth")>()),
  useAuthStore: (selector: (state: { user: { id: string } }) => unknown) =>
    selector({ user: { id: "viewer-1" } }),
}));

vi.mock("@multica/core/agents", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/agents")>()),
  isAgentRuntimeBound: () => true,
  useAgentPresenceDetail: () => ({ availability: "offline", workload: null }),
}));

vi.mock("@multica/core/paths", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@multica/core/paths")>();
  return {
    ...actual,
    useCurrentWorkspace: () => ({ id: "ws-1", slug: "acme" }),
    useWorkspacePaths: () => actual.paths.workspace("acme"),
  };
});

vi.mock("../../navigation", () => ({
  AppLink: ({ children, ...props }: React.ComponentProps<"a">) => <a {...props}>{children}</a>,
  useNavigation: () => ({
    push: () => {},
    openInNewTab: () => {},
    getShareableUrl: (path: string) => `https://app.example${path}`,
    pathname: "/",
  }),
  resolveClickIntent: () => "push",
  useIntentNavigate: () => () => {},
}));

// Capture the DndContext callbacks so a test drives dnd-kit's lifecycle:
// start, over (allowed column), over (blocked column), end.
const dnd = vi.hoisted(() => ({
  start: null as ((event: unknown) => void) | null,
  over: null as ((event: unknown) => void) | null,
  end: null as ((event: unknown) => void) | null,
}));
vi.mock("@dnd-kit/core", () => ({
  DndContext: ({ children, onDragStart, onDragEnd, onDragOver }: never) => {
    dnd.start = onDragStart;
    dnd.over = onDragOver;
    dnd.end = onDragEnd;
    return children;
  },
  DragOverlay: () => null,
  PointerSensor: class {},
  useSensor: () => ({}),
  useSensors: () => [],
  useDroppable: () => ({ setNodeRef: () => {}, isOver: false }),
  pointerWithin: vi.fn(),
  closestCenter: vi.fn(),
}));
vi.mock("@dnd-kit/sortable", () => ({
  SortableContext: ({ children }: never) => children,
  verticalListSortingStrategy: {},
  arrayMove: <T,>(items: T[]) => items,
  useSortable: () => ({
    attributes: {},
    listeners: {},
    setNodeRef: () => {},
    transform: null,
    transition: null,
    isDragging: false,
  }),
}));
vi.mock("@dnd-kit/utilities", () => ({ CSS: { Transform: { toString: () => undefined } } }));

class ObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
  takeRecords() {
    return [];
  }
}

function makeProject(): Project {
  return {
    id: PROJECT_ID,
    workspace_id: "ws-1",
    title: "Delivery project",
    description: null,
    icon: null,
    status: "in_progress",
    priority: "none",
    lead_type: null,
    lead_id: null,
    start_date: null,
    due_date: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    issue_count: 0,
    done_count: 0,
    resource_count: 0,
    workflow_id: "wf-1",
  } as Project;
}

const ISSUE = {
  id: "issue-1",
  workspace_id: "ws-1",
  number: 1,
  identifier: "MUL-1",
  title: "Card to drag",
  description: null,
  status: "todo",
  priority: "none",
  assignee_type: null,
  assignee_id: null,
  creator_type: "member",
  creator_id: "member-1",
  parent_issue_id: null,
  project_id: PROJECT_ID,
  position: 1,
  stage: null,
  start_date: null,
  due_date: null,
  labels: [],
  metadata: {},
  properties: {},
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
} as unknown as Issue;

describe("board drop over a column the workflow omits", () => {
  let queryClient: QueryClient;
  const onMoveIssue = vi.fn();

  beforeEach(() => {
    vi.stubGlobal("IntersectionObserver", ObserverStub);
    vi.stubGlobal("ResizeObserver", ObserverStub);
    vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => {
      callback(0);
      return 0;
    });
    onMoveIssue.mockClear();
    mockToastInfo.mockClear();
    mockToastError.mockClear();
    workflowState.workflows = [WORKFLOW];
    workflowState.loaded = true;
    queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    queryClient.setQueryData(projectListOptions("ws-1").queryKey, { projects: [makeProject()], total: 1 });
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  function mount(issue: Issue = ISSUE) {
    const store = getIssueSurfaceViewStore(`board-workflow-drop-${Math.floor(Math.random() * 1e9)}`);
    // Manual ordering is where the card is mirrored into the column it passes.
    store.getState().setSortBy("position");
    renderWithI18n(
      <QueryClientProvider client={queryClient}>
        <ViewStoreProvider store={store}>
          <IssueContextMenuProvider>
            <BoardView
              issues={[issue]}
              visibleStatuses={["todo", "in_progress", "in_review"]}
              onMoveIssue={onMoveIssue}
            />
          </IssueContextMenuProvider>
        </ViewStoreProvider>
      </QueryClientProvider>,
    );
  }

  function drag(path: string[]) {
    const active = { id: ISSUE.id };
    act(() => dnd.start?.({ active }));
    for (const column of path) {
      act(() => dnd.over?.({ active, over: { id: statusGroupId(column as never) } }));
    }
    const last = path[path.length - 1]!;
    act(() => dnd.end?.({ active, over: { id: statusGroupId(last as never) } }));
  }

  it("moves the card when the allowed column is the drop target", () => {
    mount();
    drag(["in_progress"]);

    expect(onMoveIssue).toHaveBeenCalledTimes(1);
    expect(onMoveIssue.mock.calls[0]![1]).toMatchObject({ status: "in_progress" });
  });

  it("does not move the card to the last allowed column when released over a blocked one", () => {
    mount();
    // in_progress is listed, in_review is not: the mirror stops in in_progress.
    drag(["in_progress", "in_review"]);

    expect(onMoveIssue).not.toHaveBeenCalled();
    expect(mockToastInfo).toHaveBeenCalledTimes(1);
  });

  it("does not hand off through the last allowed column when released over a blocked one", () => {
    workflowState.workflows = [
      {
        ...WORKFLOW,
        steps: WORKFLOW.steps.map((step) =>
          step.status_key === "in_progress" ? { ...step, handler: { type: "creator" } } : step,
        ),
      },
    ];
    mount();
    drag(["in_progress", "in_review"]);

    expect(onMoveIssue).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  // The card's own status is not in its workflow, so a return to its column is
  // not a "move into a blocked column". The mirror must follow the pointer back
  // there, and a release over that column is a drop where it started.
  describe("an issue whose own status the workflow omits", () => {
    const STRANDED = { ...ISSUE, status: "in_review" } as Issue;

    it("does not commit the allowed column it passed over when released back on its own column", () => {
      mount(STRANDED);
      drag(["in_progress", "in_review"]);

      expect(onMoveIssue).not.toHaveBeenCalled();
    });

    it("does not start the passed column's handoff when released back on its own column", () => {
      workflowState.workflows = [
        {
          ...WORKFLOW,
          steps: WORKFLOW.steps.map((step) =>
            step.status_key === "in_progress" ? { ...step, handler: { type: "creator" } } : step,
          ),
        },
      ];
      mount(STRANDED);
      drag(["in_progress", "in_review"]);

      expect(onMoveIssue).not.toHaveBeenCalled();
      expect(screen.queryByRole("dialog")).toBeNull();
    });

    it("still moves the card to an allowed column it is released over", () => {
      mount(STRANDED);
      drag(["in_progress"]);

      expect(onMoveIssue).toHaveBeenCalledTimes(1);
      expect(onMoveIssue.mock.calls[0]![1]).toMatchObject({ status: "in_progress" });
    });
  });

  // A board that has not loaded the workflows cannot tell a handoff step from a
  // plain one. The server applies the workflow's handoff either way, so a drop
  // that is not confirmed here would reassign the issue and start a run unseen.
  describe("before the workflows have loaded", () => {
    it("does not move a project card across statuses", () => {
      workflowState.loaded = false;
      workflowState.workflows = [];
      mount();
      drag(["in_progress"]);

      expect(onMoveIssue).not.toHaveBeenCalled();
    });

    it("still moves a card that belongs to no project", () => {
      workflowState.loaded = false;
      workflowState.workflows = [];
      mount({ ...ISSUE, project_id: null } as unknown as Issue);
      drag(["in_progress"]);

      expect(onMoveIssue).toHaveBeenCalledTimes(1);
    });
  });

  // The workflow list can be loaded while the project list is not. Which workflow
  // a card follows comes from its project, so an unresolved project reads as "no
  // workflow" exactly like an unloaded workflow list does.
  describe("before the project list has resolved", () => {
    const handoffWorkflow = () => [
      {
        ...WORKFLOW,
        steps: WORKFLOW.steps.map((step) =>
          step.status_key === "in_progress" ? { ...step, handler: { type: "creator" } } : step,
        ),
      },
    ];

    afterEach(() => setApiInstance(undefined as never));

    // The board reads the project list through the shared client.
    const useProjectList = (listProjects: () => Promise<unknown>) => {
      setApiInstance({ listProjects } as never);
      queryClient.removeQueries({ queryKey: projectListOptions("ws-1").queryKey });
      return listProjects;
    };

    it("does not move or hand off a project card while the project list is pending", () => {
      workflowState.workflows = handoffWorkflow();
      useProjectList(() => new Promise(() => {}));
      mount();
      drag(["in_progress"]);

      expect(onMoveIssue).not.toHaveBeenCalled();
      expect(screen.queryByText("confirm handoff")).toBeNull();
    });

    it("does not move a project card after the project list failed", async () => {
      const listProjects = vi.fn().mockRejectedValue(new Error("projects down"));
      useProjectList(listProjects);
      mount();
      await waitFor(() => expect(listProjects).toHaveBeenCalled());
      await act(async () => {
        await new Promise((resolve) => setTimeout(resolve, 20));
      });
      drag(["in_progress"]);

      expect(onMoveIssue).not.toHaveBeenCalled();
    });

    it("still moves a card that belongs to no project", () => {
      useProjectList(() => new Promise(() => {}));
      mount({ ...ISSUE, project_id: null } as unknown as Issue);
      drag(["in_progress"]);

      expect(onMoveIssue).toHaveBeenCalledTimes(1);
    });

    it("moves a project card once the project list has loaded", () => {
      mount();
      drag(["in_progress"]);

      expect(onMoveIssue).toHaveBeenCalledTimes(1);
    });
  });

  // Both lists can load and still fail to say which workflow a card follows: the
  // project list omits its project, the workflow list omits the project's named
  // workflow or is empty. That reads as "no workflow" while the server resolves
  // a handler, so the card stays put and the board offers a retry.
  describe("after a catalog that loaded without the card's workflow", () => {
    const withProjects = (projects: Project[]) =>
      queryClient.setQueryData(projectListOptions("ws-1").queryKey, { projects, total: projects.length });
    const plainDefault = { ...WORKFLOW, id: "wf-default", is_default: true };
    const retryToast = () => mockToastError.mock.calls.find(([, options]) => options?.action?.label === "Retry");

    it("does not move a card whose project is missing from the project list", () => {
      withProjects([]);
      mount();
      drag(["in_progress"]);

      expect(onMoveIssue).not.toHaveBeenCalled();
      expect(retryToast()).toBeTruthy();
    });

    it("does not move a card when the successful workflow list is empty but its project names a workflow", () => {
      workflowState.workflows = [];
      mount();
      drag(["in_progress"]);

      expect(onMoveIssue).not.toHaveBeenCalled();
      expect(retryToast()).toBeTruthy();
    });

    it("does not move a card when its project's workflow is missing from a non-empty list", () => {
      workflowState.workflows = [plainDefault];
      mount();
      drag(["in_progress"]);

      expect(onMoveIssue).not.toHaveBeenCalled();
    });

    it("moves a card whose project names no workflow when none is stored", () => {
      withProjects([{ ...makeProject(), workflow_id: null } as Project]);
      workflowState.workflows = [];
      mount();
      drag(["in_progress"]);

      expect(onMoveIssue).toHaveBeenCalledTimes(1);
      expect(retryToast()).toBeUndefined();
    });
  });

  // The confirmation names what the person previewed. The move must carry it, so
  // the server can refuse a confirmation the issue or workflow has outgrown.
  it("sends the previewed binding with a confirmed handoff move", () => {
    workflowState.workflows = [
      {
        ...WORKFLOW,
        steps: WORKFLOW.steps.map((step) =>
          step.status_key === "in_progress" ? { ...step, handler: { type: "creator" } } : step,
        ),
      },
    ];
    mount();
    drag(["in_progress"]);
    expect(onMoveIssue).not.toHaveBeenCalled();

    fireEvent.click(screen.getByText("confirm handoff"));

    expect(onMoveIssue).toHaveBeenCalledTimes(1);
    expect(onMoveIssue.mock.calls[0]![1]).toMatchObject({
      status: "in_progress",
      stop_previous_assignee_runs: true,
      handoff_preview: PREVIEW,
    });
  });
});
