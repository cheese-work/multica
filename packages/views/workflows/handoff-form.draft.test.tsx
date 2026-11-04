// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { resetAllRegisteredDrafts } from "@multica/core/drafts";
import { setCurrentWorkspace } from "@multica/core/platform";
import { buildIssueStatusCatalog } from "@multica/core/issue-statuses";
import { agentListOptions, memberListOptions } from "@multica/core/workspace/queries";
import { projectListOptions } from "@multica/core/projects/queries";
import type { IssueWorkflow } from "@multica/core/types";
import { renderWithI18n } from "../test/i18n";
import { WorkflowHandoffForm } from "./handoff-form";

vi.mock("@multica/core/api", () => ({ api: {}, clientErrorMessage: () => null }));
vi.mock("@multica/core/issue-workflows", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/issue-workflows")>()),
  useSetWorkflowHandoff: () => ({ mutateAsync: vi.fn(), isPending: false }),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/issue-statuses/hooks", () => ({ useIssueStatuses: () => buildIssueStatusCatalog([]) }));
vi.mock("../common/actor-avatar", () => ({ ActorAvatar: () => <span /> }));

const workflow = {
  id: "wf-1",
  workspace_id: "ws-1",
  name: "Delivery",
  description: "",
  is_default: false,
  steps: ["todo", "in_progress", "done"].map((status_key) => ({
    status_key,
    handler: { type: "none" as const },
    instructions: "",
  })),
  project_ids: [],
  created_at: "",
  updated_at: "",
} as unknown as IssueWorkflow;

function withInstructions(instructions: string): IssueWorkflow {
  return {
    ...workflow,
    steps: workflow.steps.map((s) => (s.status_key === "in_progress" ? { ...s, instructions } : s)),
  };
}

const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });

function tree(wf: IssueWorkflow) {
  return (
    <QueryClientProvider client={qc}>
      <WorkflowHandoffForm workflow={wf} statusKey="in_progress" canEdit />
    </QueryClientProvider>
  );
}

function mount(wf: IssueWorkflow = workflow) {
  qc.setQueryData(agentListOptions("ws-1").queryKey, [{ id: "agent-1", name: "Builder", archived_at: null }] as never);
  qc.setQueryData(memberListOptions("ws-1").queryKey, [] as never);
  qc.setQueryData(projectListOptions("ws-1").queryKey, { projects: [] } as never);
  return renderWithI18n(tree(wf));
}

beforeEach(async () => {
  localStorage.clear();
  resetAllRegisteredDrafts();
  setCurrentWorkspace("acme", "ws-1");
  await new Promise((r) => queueMicrotask(() => r(null)));
});
afterEach(cleanup);

describe("WorkflowHandoffForm draft", () => {
  it("keeps the chosen handler and typed instructions across a remount", async () => {
    const first = mount();
    fireEvent.click(screen.getByRole("radio", { name: /Hand to an agent/ }));
    fireEvent.click(screen.getByRole("button", { name: "Choose an agent" }));
    fireEvent.click(await screen.findByText("Builder"));
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "Write the test first" } });
    first.unmount();

    mount();
    expect(screen.getByRole("radio", { name: /Hand to an agent/ }).getAttribute("aria-checked")).toBe("true");
    expect(screen.getByText("Builder")).toBeTruthy();
    expect((screen.getByRole("textbox") as HTMLTextAreaElement).value).toBe("Write the test first");
  });

  it("keeps unsaved edits when the saved step refreshes underneath them", async () => {
    const view = mount();
    fireEvent.click(screen.getByRole("radio", { name: /Hand to an agent/ }));
    fireEvent.click(screen.getByRole("button", { name: "Choose an agent" }));
    fireEvent.click(await screen.findByText("Builder"));
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "My unsent plan" } });

    view.rerender(tree(withInstructions("Someone else's text")));

    expect((screen.getByRole("textbox") as HTMLTextAreaElement).value).toBe("My unsent plan");
    expect(screen.getByText(/changed by someone else/)).toBeTruthy();
    view.unmount();
    mount(withInstructions("Someone else's text"));
    expect((screen.getByRole("textbox") as HTMLTextAreaElement).value).toBe("My unsent plan");
  });

  it("follows the saved step when there are no unsaved edits", () => {
    const view = mount();
    view.rerender(tree(withInstructions("From elsewhere")));
    expect(screen.queryByText(/changed by someone else/)).toBeNull();
  });
});
