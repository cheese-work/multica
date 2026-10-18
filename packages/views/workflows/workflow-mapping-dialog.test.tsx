// @vitest-environment jsdom

import { useState } from "react";
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { buildIssueStatusCatalog } from "@multica/core/issue-statuses";
import type { IssueWorkflowMappingPlan } from "@multica/core/types";
import { renderWithI18n } from "../test/i18n";
import { WorkflowMappingDialog } from "./workflow-mapping-dialog";

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () => buildIssueStatusCatalog([]),
}));

const plan: IssueWorkflowMappingPlan = {
  required: [{ status_key: "in_review", issue_count: 3, suggested_status_key: "todo" }],
  unchanged: [],
  total_issues: 3,
  affected_issues: 3,
};

afterEach(cleanup);

// The editor builds `targetKeys` with flatMap on every render, so a parent
// render hands the dialog a new array with the same contents. That is not a
// change to the destinations, and it must not undo what the person picked.
function Harness({ onConfirm, plan: p = plan }: { onConfirm: (m: Record<string, string>) => void; plan?: IssueWorkflowMappingPlan }) {
  const [renders, setRenders] = useState(0);
  const targetKeys = ["todo", "in_progress", "done"].map((key) => key);
  return (
    <>
      <button onClick={() => setRenders(renders + 1)}>rerender parent</button>
      <WorkflowMappingDialog
        open
        onOpenChange={() => {}}
        variant={{ kind: "edit", workflowName: "Delivery" }}
        plan={p}
        targetKeys={targetKeys}
        pending={false}
        onConfirm={onConfirm}
      />
    </>
  );
}

async function pick(label: string) {
  fireEvent.click(screen.getByRole("combobox"));
  fireEvent.click(await screen.findByRole("option", { name: new RegExp(label, "i") }));
}

describe("WorkflowMappingDialog", () => {
  it("starts from each status's suggested destination", () => {
    const onConfirm = vi.fn();
    renderWithI18n(<Harness onConfirm={onConfirm} />);

    fireEvent.click(screen.getByRole("button", { name: /Save/ }));

    expect(onConfirm).toHaveBeenCalledWith({ in_review: "todo" });
  });

  it("keeps the person's choice through a parent render that rebuilds the target list", async () => {
    const onConfirm = vi.fn();
    renderWithI18n(<Harness onConfirm={onConfirm} />);
    await pick("done");

    fireEvent.click(screen.getByText("rerender parent"));
    fireEvent.click(screen.getByText("rerender parent"));
    fireEvent.click(screen.getByRole("button", { name: /Save/ }));

    await waitFor(() => expect(onConfirm).toHaveBeenCalled());
    expect(onConfirm).toHaveBeenCalledWith({ in_review: "done" });
  });

  it("falls back to a valid destination when the chosen one leaves the target list", async () => {
    const onConfirm = vi.fn();
    function Shrinking() {
      const [keys, setKeys] = useState(["todo", "in_progress", "done"]);
      return (
        <>
          <button onClick={() => setKeys(["todo", "in_progress"])}>drop done</button>
          <WorkflowMappingDialog
            open
            onOpenChange={() => {}}
            variant={{ kind: "edit", workflowName: "Delivery" }}
            plan={plan}
            targetKeys={keys}
            pending={false}
            onConfirm={onConfirm}
          />
        </>
      );
    }
    renderWithI18n(<Shrinking />);
    await pick("done");

    fireEvent.click(screen.getByText("drop done"));
    fireEvent.click(screen.getByRole("button", { name: /Save/ }));

    await waitFor(() => expect(onConfirm).toHaveBeenCalled());
    expect(onConfirm).toHaveBeenCalledWith({ in_review: "todo" });
  });
});

