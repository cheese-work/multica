/** @vitest-environment jsdom */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, within } from "@testing-library/react";
import type { WorkflowLintResponse } from "@multica/core/types";
import { renderWithI18n } from "../test/i18n";
import { WorkflowChecksSheet } from "./workflow-checks-sheet";

afterEach(cleanup);

const result: WorkflowLintResponse = {
  workflow_id: "wf-1",
  workflow: "Delivery",
  errors: 1,
  warnings: 1,
  infos: 1,
  findings: [
    { rule: "WF003", level: "error", step: "agent_accepted", message: "Agent Accepted has no exit.", hint: "Add an edge to Done." },
    { rule: "WF007", level: "warning", step: "in_review", message: "Duplicate edge to In progress.", hint: "Remove the duplicate." },
    { rule: "WF013", level: "info", message: "Used by no project.", hint: "Attach it to a project." },
    { rule: "WF002", level: "error", step: "done", message: "No handler.", hint: "", waived: true, waive_reason: "Terminal on purpose." },
  ],
};
const labels: Record<string, string> = { agent_accepted: "Agent Accepted", in_review: "In review", done: "Done" };

function mount(over: Partial<React.ComponentProps<typeof WorkflowChecksSheet>> = {}) {
  const props = {
    open: true,
    onOpenChange: vi.fn(),
    workflowName: "Delivery",
    result,
    loading: false,
    failed: false,
    checkedAt: "2026-11-19T07:02:00Z",
    stepLabel: (k: string) => labels[k] ?? k,
    onRerun: vi.fn(),
    onGoToStep: vi.fn(),
    ...over,
  };
  renderWithI18n(<WorkflowChecksSheet {...props} />);
  return props;
}

describe("WorkflowChecksSheet", () => {
  it("has filter tabs with counts, waived findings left out of the counts", () => {
    mount();
    const tabs = screen.getByRole("tablist");
    expect(within(tabs).getByRole("tab", { name: /^All\s*4$/ })).toBeTruthy();
    expect(within(tabs).getByRole("tab", { name: /^Errors\s*1$/ })).toBeTruthy();
    expect(within(tabs).getByRole("tab", { name: /^Warnings\s*1$/ })).toBeTruthy();
    expect(within(tabs).getByRole("tab", { name: /^Notes\s*1$/ })).toBeTruthy();
  });

  it("lists each finding with its message, fix hint and rule id, grouped by step", () => {
    mount();
    expect(screen.getByText("Agent Accepted has no exit.")).toBeTruthy();
    expect(screen.getByText("Add an edge to Done.")).toBeTruthy();
    expect(screen.getByText("WF003")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Go to step: Agent Accepted" })).toBeTruthy();
  });

  it("filters by level", () => {
    mount();
    fireEvent.click(screen.getByRole("tab", { name: /^Warnings/ }));
    expect(screen.getByText("Duplicate edge to In progress.")).toBeTruthy();
    expect(screen.queryByText("Agent Accepted has no exit.")).toBeNull();
  });

  it("shows a waived finding muted with its reason", () => {
    mount();
    expect(screen.getByText(/Terminal on purpose\./)).toBeTruthy();
    expect(screen.getByText(/^Waived/)).toBeTruthy();
  });

  it("goes to the step and re-runs", () => {
    const props = mount();
    fireEvent.click(screen.getByRole("button", { name: "Go to step: In review" }));
    expect(props.onGoToStep).toHaveBeenCalledWith("in_review");
    fireEvent.click(screen.getByRole("button", { name: "Re-run" }));
    expect(props.onRerun).toHaveBeenCalled();
  });

  it("stamps the check time in ICT", () => {
    mount();
    expect(screen.getByText(/14:02 ICT/)).toBeTruthy();
  });

  it("says so when the workflow is clean", () => {
    mount({ result: { ...result, findings: [], errors: 0, warnings: 0, infos: 0 } });
    expect(screen.getByText("No checks failed.")).toBeTruthy();
  });

  it("says saving stays allowed when the checks failed to run", () => {
    mount({ result: undefined, failed: true });
    expect(screen.getByText("Checks failed to run.")).toBeTruthy();
    expect(screen.getByText(/saving stays allowed/i)).toBeTruthy();
  });
});
