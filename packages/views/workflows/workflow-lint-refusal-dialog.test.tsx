/** @vitest-environment jsdom */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import type { IssueWorkflowLintFinding } from "@multica/core/types";
import { renderWithI18n } from "../test/i18n";
import { WorkflowLintRefusalDialog } from "./workflow-lint-refusal-dialog";

afterEach(cleanup);
void render;

const findings: IssueWorkflowLintFinding[] = [
  { rule: "WF003", level: "error", step: "agent_accepted", message: "Agent Accepted has no exit.", hint: "Add an edge to Done." },
  { rule: "WF001", level: "error", step: "in_review", message: "Edge points at a missing status.", hint: "Pick a defined status." },
];

function Harness({ onSaveAnyway, onBack, initial = "" }: { onSaveAnyway: (r: string) => void; onBack: () => void; initial?: string }) {
  const [reason, setReason] = useState(initial);
  return (
    <WorkflowLintRefusalDialog
      open
      findings={findings}
      reason={reason}
      onReasonChange={setReason}
      pending={false}
      stepLabel={(k) => k}
      onSaveAnyway={onSaveAnyway}
      onBack={onBack}
    />
  );
}

describe("WorkflowLintRefusalDialog", () => {
  it("lists the errors with fix hints and rule ids", () => {
    renderWithI18n(<Harness onSaveAnyway={vi.fn()} onBack={vi.fn()} />);
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(screen.getByText("Agent Accepted has no exit.")).toBeTruthy();
    expect(screen.getByText("Add an edge to Done.")).toBeTruthy();
    expect(screen.getByText("WF001")).toBeTruthy();
  });

  it("holds Save anyway until a reason is typed, and counts the reason against 500", () => {
    const onSaveAnyway = vi.fn();
    renderWithI18n(<Harness onSaveAnyway={onSaveAnyway} onBack={vi.fn()} />);
    const save = screen.getByRole("button", { name: "Save anyway" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    expect(screen.getByText("0 / 500")).toBeTruthy();
    const box = screen.getByRole("textbox", { name: "Reason for saving with errors" }) as HTMLTextAreaElement;
    expect(box.maxLength).toBe(500);
    fireEvent.change(box, { target: { value: "   " } });
    expect(save.disabled).toBe(true);
    fireEvent.change(box, { target: { value: "Fix lands next PR" } });
    expect(screen.getByText("17 / 500")).toBeTruthy();
    expect(save.disabled).toBe(false);
    fireEvent.click(save);
    expect(onSaveAnyway).toHaveBeenCalledWith("Fix lands next PR");
  });

  it("goes back to editing", () => {
    const onBack = vi.fn();
    renderWithI18n(<Harness onSaveAnyway={vi.fn()} onBack={onBack} />);
    fireEvent.click(screen.getByRole("button", { name: "Back to editing" }));
    expect(onBack).toHaveBeenCalled();
  });
});
