// @vitest-environment jsdom

import { cleanup, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import type { IssueTableQuerySpec } from "@multica/core/types";
import { renderWithI18n } from "../../../test/i18n";
import { Swimlane2View } from "./swimlane2-view";

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () => ({ entryOf: () => undefined, categoryOf: () => "started", colorOf: () => null, iconOf: () => null }),
}));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: (_type: string, id: string) => `actor ${id}` }),
}));

afterEach(cleanup);

const A = "00000000-0000-4000-8000-00000000000a";
const B = "00000000-0000-4000-8000-00000000000b";

const row = (id: string, extra: Record<string, unknown> = {}) => ({
  id,
  identifier: id === A ? "CHE-1" : "CHE-2",
  title: id === A ? "first" : "second",
  status: "in_progress",
  parent_issue_id: null,
  assignee_type: "member",
  assignee_id: id === A ? "alice" : "bob",
  project_id: id === A ? "p1" : "p2",
  ...extra,
});
const timelineIssue = (id: string) => ({
  issue_id: id,
  status: "in_progress",
  history: "complete",
  start_known: true,
  actual_start: "2026-01-01T00:00:00Z",
  actual_end: null,
  presence_from: null,
  presence_to: null,
  start_date: null,
  due_date: null,
  segments: [],
});

const query: IssueTableQuerySpec = {
  scope: { kind: "my", relation: "involved" },
  filters: { statuses: ["in_progress"] },
  sort: { field: "position", direction: "asc" },
};

function setup(overrides: Record<string, unknown> = {}) {
  const getIssueActualTimeline = vi.fn(async () => ({
    as_of: "2026-01-02T00:00:00Z",
    window: { from: "2025-12-03T00:00:00Z", to: "2026-01-02T00:00:00Z" },
    issues: [timelineIssue(A), timelineIssue(B)],
    wakeup_events: [],
    next_cursor: null,
  }));
  const getIssueRelationsBatch = vi.fn(async () => ({
    nodes: [],
    edges: [{ source: A, target: B, type: "dependency" }],
    next_cursor: null,
  }));
  const listIssues = vi.fn(async () => ({ issues: [row(A), row(B)], total: 2 }));
  setApiInstance({
    getIssueActualTimeline,
    getIssueRelationsBatch,
    listIssues,
    listProjects: async () => ({ projects: [{ id: "p1", title: "Project One" }, { id: "p2", title: "Project Two" }], total: 2 }),
    ...overrides,
  } as unknown as ApiClient);
  return { getIssueActualTimeline, getIssueRelationsBatch, listIssues };
}

function renderView(grouping: "assignee" | "project" | "parent" = "assignee") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={qc}>
      <Swimlane2View query={query} grouping={grouping} />
    </QueryClientProvider>,
  );
}

describe("Swimlane2View", () => {
  beforeEach(() => vi.clearAllMocks());

  it("sends the page's scope and filters in the timeline request", async () => {
    const api = setup();
    renderView();
    await screen.findByRole("group", { name: /CHE-1/ });
    expect(api.getIssueActualTimeline).toHaveBeenCalledTimes(1);
    const [wsId, request] = api.getIssueActualTimeline.mock.calls[0] as unknown as [string, Record<string, unknown>];
    expect(wsId).toBe("ws-1");
    expect(request.scope).toEqual({ kind: "my", relation: "involved" });
    expect(request.filters).toEqual({ statuses: ["in_progress"] });
  });

  it("renders the timeline issues as nodes with the relation edges between them", async () => {
    const api = setup();
    const { container } = renderView();
    await screen.findByRole("group", { name: /CHE-2/ });
    await waitFor(() => expect(container.querySelectorAll('[data-edge-type="dependency"]')).toHaveLength(1));
    expect(api.getIssueRelationsBatch).toHaveBeenCalled();
  });

  it("lanes follow the group-by", async () => {
    setup();
    const { unmount } = renderView("assignee");
    await screen.findByText("actor alice");
    expect(screen.getByText("actor bob")).toBeTruthy();
    unmount();
    renderView("project");
    await screen.findByText("Project One");
    expect(screen.getByText("Project Two")).toBeTruthy();
  });

  it("offers a retry and no empty graph when the timeline request fails", async () => {
    setup({ getIssueActualTimeline: vi.fn(async () => { throw new Error("boom"); }) });
    renderView();
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.queryByRole("group", { name: /CHE-1/ })).toBeNull();
  });
});
