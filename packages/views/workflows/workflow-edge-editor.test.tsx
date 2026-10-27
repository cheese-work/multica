// @vitest-environment jsdom

import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { buildIssueStatusCatalog } from "@multica/core/issue-statuses";
import type { IssueWorkflowStep } from "@multica/core/types";
import { renderWithI18n } from "../test/i18n";
import { WorkflowEdgeEditor } from "./workflow-edge-editor";

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () => buildIssueStatusCatalog([]),
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
    edges: [{ to: "in_review", guard: "agent_done" }],
  }),
  step("in_review"),
  step("blocked"),
  step("done"),
];

const lastSteps = (onChange: ReturnType<typeof vi.fn>) =>
  onChange.mock.calls.at(-1)![0] as IssueWorkflowStep[];
const edgesOf = (out: IssueWorkflowStep[], key: string) =>
  out.find((s) => s.status_key === key)!.edges;

function renderEditor(
  props: Partial<Parameters<typeof WorkflowEdgeEditor>[0]> = {},
) {
  const onChange = vi.fn();
  renderWithI18n(
    <WorkflowEdgeEditor
      steps={steps}
      stepKey="in_progress"
      canEdit
      onChange={onChange}
      {...props}
    />,
  );
  return onChange;
}

afterEach(cleanup);

describe("WorkflowEdgeEditor", () => {
  it("asks for a step when none is selected", () => {
    renderEditor({ stepKey: null });
    expect(screen.getByText(/Select a step to edit its exits/)).toBeTruthy();
  });

  it("lists the selected step's exits with their guard", () => {
    renderEditor();
    const exit = screen.getByRole("group", { name: "Exit 1: In Review" });
    expect(
      within(exit).getByRole("combobox", { name: "Guard" }).textContent,
    ).toContain("agent_done");
  });

  it("changing a guard to a PR guard adds the timeout the server needs", async () => {
    const user = userEvent.setup();
    const onChange = renderEditor();
    const exit = screen.getByRole("group", { name: "Exit 1: In Review" });
    await user.click(within(exit).getByRole("combobox", { name: "Guard" }));
    await user.click(await screen.findByRole("option", { name: "pr.merged" }));
    await waitFor(() =>
      expect(edgesOf(lastSteps(onChange), "in_progress")).toEqual([
        { to: "in_review", guard: "pr.merged", timeout: "72h", on_timeout: "blocked" },
      ]),
    );
  });

  it("offers pr.checks_green only from a status CI green may leave", async () => {
    const user = userEvent.setup();
    const open = async (stepKey: string, from: string) => {
      renderEditor({
        stepKey,
        steps: steps.map((s) =>
          s.status_key === from ? { ...s, edges: [{ to: "done", guard: "agent_done" }] } : s,
        ),
      });
      await user.click(screen.getByRole("combobox", { name: "Guard" }));
      return screen.findByRole("option", { name: "pr.merged" });
    };
    await open("in_progress", "in_progress");
    expect(screen.queryByRole("option", { name: "pr.checks_green" })).not.toBeNull();
    cleanup();
    await open("blocked", "blocked");
    expect(screen.queryByRole("option", { name: "pr.checks_green" })).toBeNull();
  });

  it("edits the timeout of a PR guard", () => {
    const onChange = renderEditor({
      steps: steps.map((s) =>
        s.status_key === "in_progress"
          ? {
              ...s,
              edges: [
                { to: "done", guard: "pr.merged", timeout: "72h", on_timeout: "blocked" },
              ],
            }
          : s,
      ),
    });
    fireEvent.change(screen.getByRole("textbox", { name: "Timeout" }), {
      target: { value: "24h" },
    });
    expect(edgesOf(lastSteps(onChange), "in_progress")![0]!.timeout).toBe("24h");
  });

  it("removes an exit", () => {
    const onChange = renderEditor();
    fireEvent.click(screen.getByRole("button", { name: "Remove exit" }));
    expect(edgesOf(lastSteps(onChange), "in_progress")).toEqual([]);
  });

  it("adds an exit to another step", async () => {
    const user = userEvent.setup();
    const onChange = renderEditor();
    await user.click(screen.getByRole("combobox", { name: "Add exit" }));
    await user.click(await screen.findByRole("option", { name: "Done" }));
    await waitFor(() =>
      expect(edgesOf(lastSteps(onChange), "in_progress")).toEqual([
        { to: "in_review", guard: "agent_done" },
        { to: "done", guard: "agent_done" },
      ]),
    );
  });

  it("is read-only without edit rights", () => {
    renderEditor({ canEdit: false });
    expect(screen.queryByRole("button", { name: "Remove exit" })).toBeNull();
    expect(screen.queryByRole("combobox", { name: "Add exit" })).toBeNull();
    expect(
      (screen.getByRole("combobox", { name: "Guard" }) as HTMLElement).hasAttribute(
        "data-disabled",
      ) ||
        screen.getByRole("combobox", { name: "Guard" }).getAttribute("aria-disabled") === "true",
    ).toBe(true);
  });
});
