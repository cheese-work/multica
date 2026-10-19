// @vitest-environment jsdom

import { cleanup, fireEvent, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { buildIssueStatusCatalog } from "@multica/core/issue-statuses";
import type { Issue, IssueWorkflow } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { IssuePropertyPills } from "./issue-property-pills";

// The board's side peek edits an existing issue through these pills. Choosing a
// status there must go through the same guard and handoff confirmation as every
// other single-issue surface.

let projectWorkflow: IssueWorkflow | null = null;
let projectResolved = true;

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () =>
    buildIssueStatusCatalog([
      { id: "ir", workspace_id: "ws-1", key: "in_review", name: "In Review", description: "", category: "started", color: "#22c55e", is_system: true, position: 0, archived_at: null, created_at: "", updated_at: "" },
    ]),
}));
vi.mock("@multica/core/issue-workflows", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/issue-workflows")>()),
  useProjectWithWorkflow: () => ({ project: null, workflow: projectWorkflow, resolved: projectResolved }),
}));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: () => "Sentinel" }),
}));
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => <span /> }));
vi.mock("../../workflows/handoff-confirm-dialog", () => ({
  WorkflowHandoffConfirmDialog: ({ issue }: { issue: unknown }) => (issue ? <div>handoff confirmation</div> : null),
}));
vi.mock("../../projects/components/project-picker", () => ({ ProjectPicker: () => <div /> }));
vi.mock("./pickers", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./pickers")>();
  const Stub = () => <div />;
  return {
    ...actual,
    PriorityPicker: Stub,
    AssigneePicker: Stub,
    DueDatePicker: Stub,
    StartDatePicker: Stub,
    LabelPicker: Stub,
  };
});

const ISSUE = {
  id: "issue-1",
  identifier: "TES-1",
  status: "todo",
  priority: "none",
  assignee_type: null,
  assignee_id: null,
  project_id: "project-1",
  start_date: null,
  due_date: null,
} as unknown as Issue;

const WORKFLOW = {
  id: "wf",
  workspace_id: "ws-1",
  name: "Delivery",
  description: "",
  is_default: false,
  project_ids: ["project-1"],
  created_at: "",
  updated_at: "",
  steps: [
    { status_key: "todo", handler: { type: "none" }, instructions: "" },
    { status_key: "in_review", handler: { type: "agent", id: "agent-1" }, instructions: "" },
  ],
} as IssueWorkflow;

function openStatusAndChoose(onUpdate: () => void) {
  renderWithI18n(<IssuePropertyPills issue={ISSUE} onUpdate={onUpdate} />);
  fireEvent.click(screen.getByRole("button", { name: /todo/i }));
  const row = Array.from(document.querySelectorAll<HTMLElement>("button[data-picker-item]")).find((el) =>
    el.textContent?.includes("In Review"),
  );
  fireEvent.click(row!);
}

afterEach(() => {
  cleanup();
  projectWorkflow = null;
  projectResolved = true;
});

describe("IssuePropertyPills status", () => {
  it("confirms a handoff step before writing", () => {
    projectWorkflow = WORKFLOW;
    const onUpdate = vi.fn();
    openStatusAndChoose(onUpdate);

    expect(onUpdate).not.toHaveBeenCalled();
    expect(screen.getByText("handoff confirmation")).toBeTruthy();
  });

  it("writes nothing while the project's workflow is unresolved", () => {
    projectResolved = false;
    const onUpdate = vi.fn();
    openStatusAndChoose(onUpdate);

    expect(onUpdate).not.toHaveBeenCalled();
  });

  it("writes a status the workflow does not hand off", () => {
    const onUpdate = vi.fn();
    openStatusAndChoose(onUpdate);

    expect(onUpdate).toHaveBeenCalledWith({ status: "in_review" });
  });
});
