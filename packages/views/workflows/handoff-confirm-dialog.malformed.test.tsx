// @vitest-environment jsdom

import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import { buildIssueStatusCatalog } from "@multica/core/issue-statuses";
import { ApiClient } from "@multica/core/api";
import { renderWithI18n } from "../test/i18n";
import { WorkflowHandoffConfirmDialog } from "./handoff-confirm-dialog";

// The real client parses the response; only the network is stubbed. A mocked
// client method would skip the parser this file exists to cover.
const realApi = vi.hoisted(() => ({ current: null as null | Pick<ApiClient, "previewWorkflowHandoff"> }));
vi.mock("@multica/core/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@multica/core/api")>();
  return {
    ...actual,
    api: {
      previewWorkflowHandoff: (...args: Parameters<ApiClient["previewWorkflowHandoff"]>) =>
        realApi.current!.previewWorkflowHandoff(...args),
    },
  };
});
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () =>
    buildIssueStatusCatalog([
      { id: "b", workspace_id: "ws-1", key: "code_review", name: "Code review", description: "", category: "started", color: "#a855f7", is_system: false, position: 2, archived_at: null, created_at: "", updated_at: "" },
    ]),
}));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: (_type: string, id: string) => id }),
}));
vi.mock("../common/actor-avatar", () => ({ ActorAvatar: () => <span /> }));

const responses: unknown[] = [];
function stubFetch() {
  const fetchMock = vi.fn(async () => {
    const body = responses.length > 1 ? responses.shift() : responses[0];
    return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function renderDialog() {
  realApi.current = new ApiClient("https://api.example.test");
  const onConfirm = vi.fn();
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithI18n(
    <QueryClientProvider client={qc}>
      <WorkflowHandoffConfirmDialog
        issue={{ id: "issue-1", identifier: "MUL-8121" }}
        toStatus="code_review"
        onCancel={vi.fn()}
        onConfirm={onConfirm}
      />
    </QueryClientProvider>,
  );
  return { onConfirm };
}

const handoff = {
  handoff: true,
  workflow_name: "Delivery",
  from_status: "implement",
  to_status: "code_review",
  handler_type: "agent",
  handler_id: "sentinel",
  previous_assignee_type: null,
  previous_assignee_id: null,
  previous_runs: [],
  brief: "Review the PR.",
};

afterEach(() => {
  cleanup();
  responses.length = 0;
  vi.unstubAllGlobals();
});

// A preview that found no handoff applies the move bound to that result.
const NO_HANDOFF_BINDING = {
  stopPreviousRuns: false,
  preview: { previous_assignee_type: null, previous_assignee_id: null, handler_type: null, handler_id: null },
};

describe("WorkflowHandoffConfirmDialog with a malformed successful preview", () => {
  it.each([
    ["a wrong-typed flag", { handoff: "yes", previous_runs: 3 }],
    ["an empty object", {}],
  ])("does not apply the move on %s, and offers a retry", async (_name, body) => {
    responses.push(body);
    stubFetch();
    const { onConfirm } = renderDialog();

    expect(await screen.findByText("Couldn’t move the issue")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    // The confirm button is disabled while the preview is unreadable, and pressing it does nothing.
    const move = screen.queryByRole("button", { name: "Move to Code review" });
    if (move) {
      expect((move as HTMLButtonElement).disabled).toBe(true);
      fireEvent.click(move);
    }
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it("asks for confirmation when the retry returns a real handoff", async () => {
    responses.push({ handoff: "yes" }, handoff);
    const fetchMock = stubFetch();
    const { onConfirm } = renderDialog();
    await screen.findByText("Couldn’t move the issue");

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    const confirm = await screen.findByRole("button", { name: "Move to Code review" });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(onConfirm).not.toHaveBeenCalled();
    fireEvent.click(confirm);
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });

  it("still applies a valid non-handoff response without asking", async () => {
    responses.push({ handoff: false });
    stubFetch();
    const { onConfirm } = renderDialog();

    await waitFor(() => expect(onConfirm).toHaveBeenCalledWith(NO_HANDOFF_BINDING));
  });

  it("still asks before applying a valid handoff", async () => {
    responses.push(handoff);
    stubFetch();
    const { onConfirm } = renderDialog();

    expect(await screen.findByRole("button", { name: "Move to Code review" })).toBeTruthy();
    expect(onConfirm).not.toHaveBeenCalled();
  });
});
