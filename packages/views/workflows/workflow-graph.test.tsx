// @vitest-environment jsdom

import { cleanup, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { buildIssueStatusCatalog } from "@multica/core/issue-statuses";
import type { IssueWorkflowStep } from "@multica/core/types";
import { renderWithI18n } from "../test/i18n";
import { WorkflowGraph } from "./workflow-graph";

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () => buildIssueStatusCatalog([]),
}));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: () => "Builder" }),
}));
vi.mock("../common/actor-avatar", () => ({
  ActorAvatar: ({ actorId }: { actorId: string }) => (
    <span data-testid={`avatar-${actorId}`} />
  ),
}));

const step = (
  status_key: string,
  rest: Partial<IssueWorkflowStep> = {},
): IssueWorkflowStep => ({
  status_key,
  handler: { type: "none" },
  instructions: "",
  ...rest,
});

const steps = [
  step("todo"),
  step("in_progress", {
    handler: { type: "agent", id: "agent-1" },
    instructions: "",
    next_status_key: "in_review",
  }),
  step("in_review", {
    back_status_key: "in_progress",
    edges: [
      { to: "done", guard: "pr.merged", timeout: "72h", on_timeout: "blocked" },
    ],
  }),
  step("done"),
];

afterEach(cleanup);

describe("WorkflowGraph", () => {
  it("lists every step and every exit with its guard", () => {
    renderWithI18n(
      <WorkflowGraph
        workflow={{ steps }}
        currentStatus="in_progress"
        project={null}
      />,
    );
    const list = screen.getByRole("list", { name: /Workflow exits/ });
    const items = within(list)
      .getAllByRole("listitem")
      .map((li) => li.textContent);
    expect(items).toEqual([
      expect.stringMatching(/In Progress.*In Review.*agent_done/),
      expect.stringMatching(/In Review.*Done.*pr\.merged.*72h/),
    ]);
  });

  it("marks the issue's current step", () => {
    renderWithI18n(
      <WorkflowGraph
        workflow={{ steps }}
        currentStatus="in_progress"
        project={null}
      />,
    );
    expect(screen.getByRole("img", { name: /Workflow graph/ })).toBeTruthy();
    expect(
      within(screen.getByTestId("workflow-graph"))
        .getByText("In Progress")
        .closest("[aria-current='step']"),
    ).toBeTruthy();
    expect(
      within(screen.getByTestId("workflow-graph"))
        .getByText("In Review")
        .closest("[aria-current='step']"),
    ).toBeNull();
  });
});
