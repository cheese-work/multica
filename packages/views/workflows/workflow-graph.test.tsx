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

  it("highlights the exit a history entry took, and only that one", () => {
    renderWithI18n(
      <WorkflowGraph
        workflow={{ steps }}
        project={null}
        highlightEdge={{ from: "in_review", to: "done" }}
      />,
    );
    const list = screen.getByRole("list", { name: /Workflow exits/ });
    const current = within(list)
      .getAllByRole("listitem")
      .filter((li) => li.getAttribute("aria-current") === "true");
    expect(current.map((li) => li.textContent)).toEqual([
      expect.stringMatching(/In Review.*Done/),
    ]);
  });

  it("draws no edge when the highlighted change followed none", () => {
    renderWithI18n(
      <WorkflowGraph
        workflow={{ steps }}
        project={null}
        highlightEdge={{ from: "todo", to: "done" }}
      />,
    );
    const list = screen.getByRole("list", { name: /Workflow exits/ });
    expect(
      within(list)
        .getAllByRole("listitem")
        .filter((li) => li.getAttribute("aria-current") === "true"),
    ).toEqual([]);
    expect(within(list).getAllByRole("listitem")).toHaveLength(2);
  });

  describe("PR merge rule", () => {
    const render = (prMergeStatus?: string) =>
      renderWithI18n(
        <WorkflowGraph workflow={{ steps }} project={null} prMergeStatus={prMergeStatus} />,
      );
    const exits = () =>
      within(screen.getByRole("list", { name: /Workflow exits/ }))
        .getAllByRole("listitem")
        .map((li) => li.textContent);

    it("draws the rule from an Any step marker, labelled PR merged", () => {
      render("done");
      expect(exits()).toContain("Any step to Done when PR merged");
      expect(within(screen.getByTestId("workflow-graph")).getByText("Any step")).toBeTruthy();
    });

    it("keeps the marker out of the tab order and exposes it in the exits list", () => {
      render("done");
      const marker = within(screen.getByTestId("workflow-graph")).getByText("Any step");
      expect(marker.closest("button, a, [tabindex]")).toBeNull();
      expect(exits()).toContain("Any step to Done when PR merged");
    });

    it("tells the target step where the rule is set", () => {
      render("done");
      const card = within(screen.getByTestId("workflow-graph"))
        .getByText("Done")
        .closest("div[style]") as HTMLElement;
      expect(within(card).getByText("Set in Settings → Code")).toBeTruthy();
    });

    it("draws nothing for none or a status the workflow does not list", () => {
      render("none");
      expect(exits()).toHaveLength(2);
      cleanup();
      render("merged");
      expect(exits()).toHaveLength(2);
      expect(screen.queryByText("Any step")).toBeNull();
      expect(screen.queryByText("Set in Settings → Code")).toBeNull();
    });

    it("tags a stored pr.merged exit deprecated while the rule is set", () => {
      render("done");
      expect(exits().some((x) => /pr\.merged \(deprecated\)/.test(x ?? ""))).toBe(true);
      cleanup();
      render("none");
      expect(exits().some((x) => /deprecated/.test(x ?? ""))).toBe(false);
    });
  });
});
