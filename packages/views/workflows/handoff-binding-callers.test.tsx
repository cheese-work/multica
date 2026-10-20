// @vitest-environment jsdom

import { cleanup, fireEvent, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import { renderWithI18n } from "../test/i18n";
import { WorkflowHandoffModal } from "../modals/workflow-handoff";

// What the confirmation dialog hands back, standing in for a fetched preview.
const PREVIEW = {
  previous_assignee_type: "agent",
  previous_assignee_id: "forge",
  handler_type: "agent",
  handler_id: "sentinel",
};
vi.mock("./handoff-confirm-dialog", () => ({
  WorkflowHandoffConfirmDialog: ({ issue, onConfirm }: { issue: unknown; onConfirm: (o: unknown) => void }) =>
    issue ? <button onClick={() => onConfirm({ stopPreviousRuns: true, preview: PREVIEW })}>confirm</button> : null,
}));

const mutate = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/issues/mutations", () => ({ useUpdateIssue: () => ({ mutate }) }));

afterEach(() => {
  cleanup();
  mutate.mockReset();
});

describe("callers of the handoff confirmation", () => {
  it("the context-menu modal sends the previewed binding with the status change", () => {
    renderWithI18n(
      <QueryClientProvider client={new QueryClient()}>
        <WorkflowHandoffModal onClose={() => {}} data={{ issueId: "issue-1", identifier: "MUL-1", status: "in_progress" }} />
      </QueryClientProvider>,
    );

    fireEvent.click(screen.getByText("confirm"));

    expect(mutate.mock.calls[0]![0]).toMatchObject({
      id: "issue-1",
      status: "in_progress",
      stop_previous_assignee_runs: true,
      handoff_preview: PREVIEW,
    });
  });
});
