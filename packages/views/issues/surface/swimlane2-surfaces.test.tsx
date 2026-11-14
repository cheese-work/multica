// @vitest-environment jsdom
// CHE-4312 W3 surface tests: Swimlane 2.0 on Project, Issues and My Issues (the real mode list of each page).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import { pruneIssueSurfaceViewStates } from "@multica/core/issues/stores/surface-view-store";
import type { IssueScope } from "@multica/core/issues/surface/scope";
import { renderWithI18n } from "../../test/i18n";
import { IssueDisplayControls } from "../components/issues-header";
import { IssueSurface } from "./issue-surface";
import { MY_ISSUE_MODES, PROJECT_ISSUE_MODES, WORKSPACE_ISSUE_MODES } from "./modes";
import { statusTableMethodsFromLegacy } from "./status-table-test-api";
import type { IssueSurfaceMode } from "./types";

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("react-virtuoso", () => ({
  Virtuoso: ({ data, itemContent }: any) => <div>{(data ?? []).map((item: any, i: number) => <div key={i}>{itemContent(i, item)}</div>)}</div>,
}));
vi.mock("@tanstack/react-virtual", () => ({
  useVirtualizer: ({ count, estimateSize }: any) => ({
    getVirtualItems: () => Array.from({ length: count }, (_, index) => ({ index, start: index * estimateSize(), end: (index + 1) * estimateSize() })),
    getTotalSize: () => count * estimateSize(),
  }),
}));
vi.mock("@multica/core/auth", () => {
  const state = { user: { id: "user-1", email: "t@t.com", name: "T" }, isAuthenticated: true };
  return {
    useAuthStore: Object.assign((selector?: (s: unknown) => unknown) => (selector ? selector(state) : state), { getState: () => state }),
    registerAuthStore: vi.fn(),
    createAuthStore: vi.fn(),
  };
});
vi.mock("../../navigation", () => ({
  AppLink: ({ children, href, ...props }: React.ComponentProps<"a">) => <a href={href} {...props}>{children}</a>,
  useNavigation: () => ({ push: vi.fn(), pathname: "/" }),
  resolveClickIntent: () => "push",
  useIntentNavigate: () => () => {},
}));
vi.mock("@multica/core/paths", async () => {
  const actual = await vi.importActual<typeof import("@multica/core/paths")>("@multica/core/paths");
  return { ...actual, useCurrentWorkspace: () => ({ id: "ws-1", name: "WS", slug: "test" }), useWorkspacePaths: () => actual.paths.workspace("test") };
});

const A = "00000000-0000-4000-8000-00000000000a";
const never = <T,>() => new Promise<T>(() => {});

let getIssueActualTimeline: ReturnType<typeof vi.fn>;

beforeEach(() => {
  getIssueActualTimeline = vi.fn(async () => ({
    as_of: "2026-01-02T00:00:00Z",
    window: { from: "2025-12-03T00:00:00Z", to: "2026-01-02T00:00:00Z" },
    issues: [{ issue_id: A, status: "todo", history: "complete", start_known: true, actual_start: "2026-01-01T00:00:00Z", actual_end: null, presence_from: null, presence_to: null, start_date: null, due_date: null, segments: [] }],
    wakeup_events: [],
    next_cursor: null,
  }));
  const listIssues = vi.fn(async (params?: { ids?: string[] }) => ({
    issues: params?.ids ? [{ id: A, identifier: "CHE-1", title: "first", status: "todo", parent_issue_id: null, assignee_type: null, assignee_id: null, project_id: null }] : [],
    total: params?.ids ? 1 : 0,
  }));
  setApiInstance({
    listIssueStatuses: async () => ({ statuses: [], categories: [], total: 0 }),
    listIssues,
    ...statusTableMethodsFromLegacy(listIssues as never),
    listGroupedIssues: vi.fn(() => never()),
    listProjects: async () => ({ projects: [], total: 0 }),
    getAgentTaskSnapshot: vi.fn(() => never()),
    getChildIssueProgress: vi.fn(() => never()),
    getIssueActualTimeline,
    getIssueRelationsBatch: vi.fn(async () => ({ nodes: [], edges: [], next_cursor: null })),
  } as unknown as ApiClient);
  pruneIssueSurfaceViewStates([]);
});
afterEach(() => {
  cleanup();
  pruneIssueSurfaceViewStates([]);
});

function surface(scope: IssueScope, modes: IssueSurfaceMode[], qc: QueryClient) {
  return (
    <QueryClientProvider client={qc}>
      <IssueSurface
        scope={scope}
        modes={modes}
        batchToolbar="never"
        renderHeader={({ controller }) => (
          <IssueDisplayControls scopedIssues={controller.surfaceIssues} allowGantt={controller.allowGantt} allowSwimlane2={controller.allowSwimlane2} />
        )}
      />
    </QueryClientProvider>
  );
}

async function openPopup(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole("button", { name: "Display" }));
  return within(await screen.findByRole("group", { name: "View" }));
}

const surfaces = [
  { name: "Issues", scope: { type: "workspace", actorKind: "all" } as IssueScope, modes: WORKSPACE_ISSUE_MODES, scopeOnWire: { kind: "workspace" }, gantt: false },
  { name: "My Issues", scope: { type: "my", userId: "user-1", relation: "assigned" } as IssueScope, modes: MY_ISSUE_MODES, scopeOnWire: { kind: "my", relation: "assigned" }, gantt: false },
  { name: "Project", scope: { type: "project", projectId: "p1" } as IssueScope, modes: PROJECT_ISSUE_MODES, scopeOnWire: { kind: "project", project_id: "p1" }, gantt: true },
];

describe.each(surfaces)("Swimlane 2.0 on $name", ({ scope, modes, scopeOnWire, gantt }) => {
  it("lists Swimlane 2.0 first and leaves the upstream modes in place", async () => {
    const user = userEvent.setup();
    renderWithI18n(surface(scope, modes, new QueryClient({ defaultOptions: { queries: { retry: false } } })));
    const group = await openPopup(user);
    const labels = group.getAllByRole("button").map((b) => b.textContent);
    expect(labels).toEqual(["Swimlane 2.0", "Board", "List", "Table", "Swimlane", ...(gantt ? ["Gantt"] : [])]);
  });

  it("selecting it renders the new renderer and sends the page scope in the timeline request", async () => {
    const user = userEvent.setup();
    renderWithI18n(surface(scope, modes, new QueryClient({ defaultOptions: { queries: { retry: false } } })));
    const group = await openPopup(user);
    expect(getIssueActualTimeline).not.toHaveBeenCalled();
    await user.click(group.getByRole("button", { name: "Swimlane 2.0" }));
    await screen.findByRole("group", { name: /CHE-1/ });
    expect(getIssueActualTimeline).toHaveBeenCalledTimes(1);
    const request = getIssueActualTimeline.mock.calls[0]![1];
    expect(request.scope).toEqual(scopeOnWire);
  });
});

describe("Swimlane 2.0 on My Issues relation switch", () => {
  it("sends a new request with the new relation", async () => {
    const user = userEvent.setup();
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const mine = (relation: "assigned" | "involved") => surface({ type: "my", userId: "user-1", relation }, MY_ISSUE_MODES, qc);
    const { rerender } = renderWithI18n(mine("assigned"));
    await user.click((await openPopup(user)).getByRole("button", { name: "Swimlane 2.0" }));
    await waitFor(() => expect(getIssueActualTimeline).toHaveBeenCalledTimes(1));
    rerender(mine("involved"));
    await user.click((await openPopup(user)).getByRole("button", { name: "Swimlane 2.0" }));
    await waitFor(() => expect(getIssueActualTimeline).toHaveBeenCalledTimes(2));
    expect(getIssueActualTimeline.mock.calls.map((c) => c[1].scope)).toEqual([
      { kind: "my", relation: "assigned" },
      { kind: "my", relation: "involved" },
    ]);
  });
});
