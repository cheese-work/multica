// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { IssueWorkflow, IssueWorkflowStep } from "../types";
import {
  issueStatusWorkflow,
  projectMoveMayHandOff,
  resolveIssueWorkflow,
  resolveProjectWorkflow,
  sharedWorkflowStatusKeys,
  stepHandsOff,
  workflowIdForTitle,
  statusAcceptsHandoff,
  workflowAllowsStatus,
  workflowMiddleKeys,
  workflowNextStatus,
  workflowStep,
} from "./queries";

const manual = (status_key: string): IssueWorkflowStep => ({ status_key, handler: { type: "none" }, instructions: "" });

const columns = (...middle: string[]) =>
  ["backlog", "todo", ...middle, "blocked", "done", "cancelled"].map(manual);

const defaultWorkflow: IssueWorkflow = {
  id: "wf-default",
  workspace_id: "ws-1",
  name: "Default",
  description: "",
  is_default: true,
  owner_id: null,
  steps: columns("in_progress", "in_review"),
  project_ids: [],
  created_at: "",
  updated_at: "",
};

const content: IssueWorkflow = {
  ...defaultWorkflow,
  id: "wf-content",
  name: "Content",
  is_default: false,
  owner_id: null,
  steps: columns("drafting", "editing"),
};

const lead: IssueWorkflowStep = { status_key: "in_review", handler: { type: "project_lead" }, instructions: "Review it." };

describe("project workflow resolution (MUL-7420)", () => {
  it("uses the project's workflow, else Default", () => {
    expect(resolveProjectWorkflow([defaultWorkflow, content], { workflow_id: "wf-content" })?.id).toBe("wf-content");
    expect(resolveProjectWorkflow([defaultWorkflow, content], { workflow_id: null })?.id).toBe("wf-default");
    // A dangling reference renders as Default, like the server treats it.
    expect(resolveProjectWorkflow([defaultWorkflow, content], { workflow_id: "gone" })?.id).toBe("wf-default");
  });

  it("is unknown while workflows load or without a project", () => {
    expect(resolveProjectWorkflow(undefined, { workflow_id: null })).toBeNull();
    expect(resolveProjectWorkflow([defaultWorkflow], undefined)).toBeNull();
    expect(resolveProjectWorkflow([], { workflow_id: null })).toBeNull();
  });

  it("carries the workflow's own handoffs", () => {
    const reviewed = { ...content, steps: content.steps.map((s) => (s.status_key === "editing" ? { ...lead, status_key: "editing" } : s)) };
    const resolved = resolveProjectWorkflow([defaultWorkflow, reviewed], { workflow_id: "wf-content" });
    expect(stepHandsOff(workflowStep(resolved, "editing"))).toBe(true);
    expect(workflowStep(resolved, "editing")?.instructions).toBe("Review it.");
    expect(stepHandsOff(workflowStep(resolved, "drafting"))).toBe(false);
  });
});

describe("issue status workflow (MUL-8250)", () => {
  const projects = [
    { id: "p-content", workflow_id: "wf-content" },
    { id: "p-default", workflow_id: null },
  ];

  it("uses Default outside a project and the project's workflow inside one", () => {
    const workflows = [defaultWorkflow, content];
    expect(issueStatusWorkflow(workflows, projects, { project_id: null })?.id).toBe("wf-default");
    expect(issueStatusWorkflow(workflows, projects, { project_id: "p-content" })?.id).toBe("wf-content");
    expect(issueStatusWorkflow(workflows, projects, { project_id: "p-default" })?.id).toBe("wf-default");
    expect(issueStatusWorkflow(workflows, undefined, { project_id: "p-content" })).toBeNull();
    expect(issueStatusWorkflow(undefined, projects, { project_id: null })).toBeNull();
  });

  it("limits a batch to the statuses every issue's workflow lists, Default included", () => {
    const workflows = [defaultWorkflow, content];
    const outside = sharedWorkflowStatusKeys(workflows, projects, [{ project_id: null }]);
    expect(outside && [...outside]).toEqual(["backlog", "todo", "in_progress", "in_review", "blocked", "done", "cancelled"]);
    const mixed = sharedWorkflowStatusKeys(workflows, projects, [{ project_id: null }, { project_id: "p-content" }]);
    expect(mixed && [...mixed]).toEqual(["backlog", "todo", "blocked", "done", "cancelled"]);
    expect(sharedWorkflowStatusKeys(undefined, projects, [{ project_id: null }])).toBeNull();
  });
});

describe("workflow helpers (MUL-7420)", () => {
  it("allows only the statuses a workflow lists, Default included", () => {
    expect(workflowAllowsStatus(null, "drafting")).toBe(true);
    expect(workflowAllowsStatus(defaultWorkflow, "drafting")).toBe(false);
    expect(workflowAllowsStatus(defaultWorkflow, "in_review")).toBe(true);
    expect(workflowAllowsStatus(content, "drafting")).toBe(true);
    expect(workflowAllowsStatus(content, "in_progress")).toBe(false);
  });

  it("names the middle columns and who may hand off", () => {
    expect(workflowMiddleKeys(content)).toEqual(["drafting", "editing"]);
    expect(statusAcceptsHandoff("todo")).toBe(true);
    expect(statusAcceptsHandoff("blocked")).toBe(true);
    expect(statusAcceptsHandoff("backlog")).toBe(false);
    expect(statusAcceptsHandoff("done")).toBe(false);
  });

  it("points a step at the next column unless it names its own", () => {
    expect(workflowNextStatus(content, "drafting")).toBe("editing");
    // blocked is a detour, not the next step.
    expect(workflowNextStatus(content, "editing")).toBe("done");
    const own = { ...content, steps: content.steps.map((s) => (s.status_key === "drafting" ? { ...s, next_status_key: "done" } : s)) };
    expect(workflowNextStatus(own, "drafting")).toBe("done");
    expect(workflowNextStatus(content, "cancelled")).toBe("");
    expect(workflowNextStatus(content, "missing")).toBe("");
  });
});

describe("projectMoveMayHandOff", () => {
  const handsOffDrafting: IssueWorkflow = {
    ...content,
    steps: content.steps.map((s) => (s.status_key === "drafting" ? { ...s, handler: { type: "agent", id: "a-1" } } : s)),
  };

  it("checks the column the issue keeps", () => {
    expect(projectMoveMayHandOff(handsOffDrafting, "drafting")).toBe(true);
    expect(projectMoveMayHandOff(handsOffDrafting, "todo")).toBe(false);
  });

  it("asks the server when the issue lands on another column", () => {
    expect(projectMoveMayHandOff(handsOffDrafting, "in_progress")).toBe(true);
    expect(projectMoveMayHandOff(content, "in_progress")).toBe(false);
  });

  it("never asks without a workflow", () => {
    expect(projectMoveMayHandOff(null, "todo")).toBe(false);
  });

  it("keeps the issue's status on a move into Default, listed or not", () => {
    const reviewing = { ...defaultWorkflow, steps: defaultWorkflow.steps.map((s) => (s.status_key === "in_review" ? lead : s)) };
    expect(projectMoveMayHandOff(reviewing, "drafting")).toBe(false);
    expect(projectMoveMayHandOff(reviewing, "in_review")).toBe(true);
  });
});

describe("per-issue workflow override", () => {
  const project = { id: "p1", workflow_id: "wf-content" } as never;
  const workflows = [defaultWorkflow, content];

  it("resolves the issue's own workflow ahead of its project's, in a project or not", () => {
    expect(resolveIssueWorkflow(workflows, [project], "p1", "wf-default")).toMatchObject({ resolved: true, workflow: { id: "wf-default" } });
    expect(resolveIssueWorkflow(workflows, [], null, "wf-content")).toMatchObject({ resolved: true, workflow: { id: "wf-content" } });
    expect(resolveIssueWorkflow(workflows, [project], "p1", null).workflow?.id).toBe("wf-content");
  });

  it("leaves an override the list lacks unresolved instead of using the project's", () => {
    expect(resolveIssueWorkflow(workflows, [project], "p1", "gone")).toMatchObject({ resolved: false, workflow: null });
  });

  it("offers no status for an override the workflow list lacks, instead of the project's", () => {
    const keys = sharedWorkflowStatusKeys(workflows, [project], [{ project_id: "p1", workflow_id: "gone" }]);
    expect(keys).not.toBeNull();
    expect(keys?.size).toBe(0);
  });

  it("judges an issue outside a project by its override, not the stored Default (CHE-3459)", () => {
    expect(issueStatusWorkflow(workflows, [project], { project_id: null, workflow_id: "wf-content" })?.id).toBe("wf-content");
    expect(issueStatusWorkflow(workflows, [project], { project_id: null, workflow_id: "gone" })).toBeNull();
    const keys = sharedWorkflowStatusKeys(workflows, [project], [{ project_id: null, workflow_id: "wf-content" }]);
    expect(keys?.has("drafting")).toBe(true);
    expect(keys?.has("in_progress")).toBe(false);
  });

  it("intersects statuses per issue, so two issues of one project can differ", () => {
    const issues = [
      { project_id: "p1", workflow_id: null },
      { project_id: "p1", workflow_id: "wf-narrow" },
    ];
    const narrow: IssueWorkflow = { ...content, id: "wf-narrow", name: "Narrow", steps: columns("editing") };
    expect([...(sharedWorkflowStatusKeys([...workflows, narrow], [project], issues) ?? [])].sort()).toEqual(
      ["backlog", "blocked", "cancelled", "done", "editing", "todo"],
    );
    expect(sharedWorkflowStatusKeys([...workflows, narrow], [project], [issues[0]!])?.has("drafting")).toBe(true);
  });
});

describe("workflowIdForTitle", () => {
  const design = "8394690d-632a-4b15-a333-c9f0247b0985";
  const settings = { workflow_by_type: { Design: design, plan: "not-a-uuid" } };

  it.each([
    ["design(x): new screen", design],
    ["Design: new screen", design],
    ["design!: breaking", design],
    ["plan(x): thing", null],
    ["feat(x): thing", null],
    ["design the thing", null],
    ["no type", null],
  ])("%s", (title, want) => {
    expect(workflowIdForTitle(settings, title)).toBe(want);
  });

  it.each([undefined, {}, { workflow_by_type: null }, { workflow_by_type: "x" }, { workflow_by_type: [design] }])(
    "no usable map: %j",
    (s) => {
      expect(workflowIdForTitle(s as Record<string, unknown> | undefined, "design: x")).toBeNull();
    },
  );

  it("resolves keys that differ only by case in sorted order, like the server", () => {
    const a = "00000000-0000-0000-0000-000000000001";
    const b = "00000000-0000-0000-0000-000000000002";
    expect(workflowIdForTitle({ workflow_by_type: { Design: b, design: a } }, "design: x")).toBe(b);
  });
});
