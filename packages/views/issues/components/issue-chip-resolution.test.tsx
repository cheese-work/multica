import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { QueryClientProvider, focusManager } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "@multica/core/api";
import { createQueryClient } from "@multica/core/query-client";
import { issueKeys } from "@multica/core/issues/queries";
import { onIssueCreated } from "@multica/core/issues/ws-updaters";
import type { Issue, ListIssuesCache } from "@multica/core/types";
import { IssueChip } from "./issue-chip";

vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: { getIssue: vi.fn() },
}));

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "workspace-1",
}));

vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () => ({ colorOf: () => null, iconOf: () => null }),
}));

vi.mock("./status-icon", () => ({
  StatusIcon: () => <svg data-testid="status-icon" />,
}));

const issue: Issue = {
  id: "11111111-1111-4111-8111-111111111111",
  workspace_id: "workspace-1",
  number: 7,
  identifier: "MUL-7",
  title: "Newly created issue",
  description: null,
  status: "todo",
  priority: "none",
  assignee_type: null,
  assignee_id: null,
  creator_type: "agent",
  creator_id: "agent-1",
  parent_issue_id: null,
  project_id: null,
  position: 0,
  stage: null,
  start_date: null,
  due_date: null,
  metadata: {},
  properties: {},
  created_at: "2026-10-12T12:32:43Z",
  updated_at: "2026-10-12T12:32:43Z",
};

describe("IssueChip resolution", () => {
  let queryClient: ReturnType<typeof createQueryClient>;

  beforeEach(() => {
    queryClient = createQueryClient();
    queryClient.setDefaultOptions({
      queries: { ...queryClient.getDefaultOptions().queries, retryDelay: 0 },
    });
    queryClient.setQueryData<ListIssuesCache>(
      issueKeys.listSorted(issue.workspace_id),
      { byStatus: { unstarted: { issues: [], total: 0 } } },
    );
    vi.mocked(api.getIssue).mockReset().mockResolvedValue(issue);
    focusManager.setFocused(true);
  });

  afterEach(() => {
    cleanup();
    queryClient.clear();
    focusManager.setFocused(undefined);
    vi.restoreAllMocks();
  });

  function chip(issueId = issue.id) {
    return (
      <QueryClientProvider client={queryClient}>
        <IssueChip issueId={issueId} fallbackLabel={issue.identifier} />
      </QueryClientProvider>
    );
  }

  it("hydrates a list miss when the detail request succeeds", async () => {
    render(chip());

    expect(screen.getByText(issue.identifier)).toBeInTheDocument();
    await screen.findByText(issue.title);
    expect(screen.getByTestId("status-icon")).toBeInTheDocument();
    expect(api.getIssue).toHaveBeenCalledExactlyOnceWith(issue.id);
  });

  it("does not refetch a successfully hydrated chip when the chat regains focus", async () => {
    render(chip());
    await screen.findByText(issue.title);

    act(() => focusManager.setFocused(false));
    act(() => focusManager.setFocused(true));
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0));
    });

    expect(api.getIssue).toHaveBeenCalledExactlyOnceWith(issue.id);
  });

  it("resolves the complete id after an earlier partial id fails", async () => {
    const partialId = issue.id.slice(0, 13);
    vi.mocked(api.getIssue).mockImplementation(async (issueId) => {
      if (issueId === partialId) throw new ApiError("Not found", 404, "Not Found");
      return issue;
    });
    const view = render(chip(partialId));
    await waitFor(() => {
      expect(
        queryClient.getQueryState(issueKeys.detail(issue.workspace_id, partialId))?.status,
      ).toBe("error");
    });

    view.rerender(chip());

    await screen.findByText(issue.title);
    expect(api.getIssue).toHaveBeenCalledWith(issue.id);
  });

  it("hydrates a failed detail request from the issue:created list update", async () => {
    vi.mocked(api.getIssue).mockRejectedValue(
      new ApiError("Unavailable", 503, "Unavailable"),
    );
    render(chip());
    await waitFor(() => {
      expect(
        queryClient.getQueryState(issueKeys.detail(issue.workspace_id, issue.id))?.status,
      ).toBe("error");
    });

    act(() => onIssueCreated(queryClient, issue.workspace_id, issue));

    await screen.findByText(issue.title);
    expect(screen.getByTestId("status-icon")).toBeInTheDocument();
  });

  it("retries an exhausted transient detail failure when the chat regains focus", async () => {
    vi.mocked(api.getIssue).mockRejectedValue(
      new ApiError("Unavailable", 503, "Unavailable"),
    );
    render(chip());
    await waitFor(() => {
      expect(
        queryClient.getQueryState(issueKeys.detail(issue.workspace_id, issue.id))?.status,
      ).toBe("error");
    });
    expect(api.getIssue).toHaveBeenCalledTimes(2);
    vi.mocked(api.getIssue).mockResolvedValue(issue);

    act(() => focusManager.setFocused(false));
    act(() => focusManager.setFocused(true));

    await screen.findByText(issue.title);
    expect(screen.getByTestId("status-icon")).toBeInTheDocument();
    expect(api.getIssue).toHaveBeenCalledTimes(3);
  });
});
