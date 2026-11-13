import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { WorkflowPill } from "./workflow-pill";

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/issue-workflows", () => ({
  useIssueWorkflows: () => ({ workflows: [{ id: "w-1", name: "Hotfix flow" }] }),
}));

describe("WorkflowPill", () => {
  it("names the workflow an issue overrides its project with", () => {
    render(<WorkflowPill workflowId="w-1" />);
    expect(screen.getByText("Hotfix flow")).toBeInTheDocument();
  });

  it("renders nothing for an issue that follows its project", () => {
    const { container } = render(<WorkflowPill workflowId={null} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing while the override's workflow is unknown", () => {
    const { container } = render(<WorkflowPill workflowId="w-gone" />);
    expect(container).toBeEmptyDOMElement();
  });
});
