import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import { NavigationProvider } from "../../navigation";
import { IssueDetailRoute } from "./issue-detail-route";

vi.mock("./issue-detail", async () => {
  const actual = await vi.importActual<typeof import("./issue-detail")>("./issue-detail");
  return { ...actual, IssueDetail: vi.fn(() => null) };
});

// The shared run UI: one dialog per run, open when asked to be.
vi.mock("../../common/task-transcript", () => ({
  TranscriptButton: ({ task, open, onOpenChange }: { task: { id: string }; open?: boolean; onOpenChange?: (open: boolean) => void }) =>
    open ? <button data-testid="run-transcript" data-task={task.id} onClick={() => onOpenChange?.(false)} /> : null,
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/workspace/hooks", () => ({ useActorName: () => ({ getActorName: () => "Sol" }) }));
vi.mock("@multica/core/paths", async () => {
  const actual = await vi.importActual<typeof import("@multica/core/paths")>("@multica/core/paths");
  return {
    ...actual,
    useCurrentWorkspace: () => ({ id: "ws-1", name: "Acme", slug: "acme" }),
    useWorkspacePaths: () => actual.paths.workspace("acme"),
  };
});

const issueId = "cb240efb-154c-42a8-ae92-42b02676feca";
const replace = vi.fn();

describe("IssueDetailRoute run deep link", () => {
  let qc: QueryClient;

  beforeEach(() => {
    replace.mockClear();
    setApiInstance({
      getIssue: vi.fn().mockResolvedValue({ id: issueId, identifier: "TRS-134" }),
      listTasksByIssue: vi.fn().mockResolvedValue([
        { id: "task-1", agent_id: "agent-1", issue_id: issueId },
        { id: "task-2", agent_id: "agent-2", issue_id: issueId },
      ]),
    } as unknown as ApiClient);
    qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } });
  });
  afterEach(() => qc.clear());

  function route(search: string, routeId = "TRS-134") {
    return (
      <QueryClientProvider client={qc}>
        <NavigationProvider
          value={{
            push: vi.fn(), replace, back: vi.fn(),
            pathname: `/acme/issues/${routeId}`,
            searchParams: new URLSearchParams(search),
            hash: "",
            getShareableUrl: (p: string) => `https://app.multica.com${p}`,
          }}
        >
          <IssueDetailRoute routeId={routeId} />
        </NavigationProvider>
      </QueryClientProvider>
    );
  }

  it("opens the run named by ?run=", async () => {
    render(route("run=task-2"));
    expect((await screen.findByTestId("run-transcript")).dataset.task).toBe("task-2");
  });

  it("opens nothing for a run that is not on the issue, or no ?run=", async () => {
    const { rerender } = render(route("run=other-issue-task"));
    await waitFor(() => expect(replace).not.toHaveBeenCalled());
    expect(screen.queryByTestId("run-transcript")).toBeNull();
    rerender(route(""));
    expect(screen.queryByTestId("run-transcript")).toBeNull();
  });

  it("keeps ?run= when it canonicalizes a UUID route", async () => {
    render(route("run=task-2", issueId));
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/acme/issues/TRS-134?run=task-2"));
  });

  it("drops ?run= from the address when the dialog closes", async () => {
    render(route("run=task-1"));
    (await screen.findByTestId("run-transcript")).click();
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/acme/issues/TRS-134"));
    expect(screen.queryByTestId("run-transcript")).toBeNull();
  });
});
