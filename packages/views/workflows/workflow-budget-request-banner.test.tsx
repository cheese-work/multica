/**
 * @vitest-environment jsdom
 *
 * Editor banner for a pending `workflow.budget.set` card (CHE-5285). It shows
 * only for a card that targets this workflow, and names the proposed cap.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import type { OpenDecisionRequest } from "@multica/core/types";
import { NavigationProvider, type NavigationAdapter } from "../navigation";
import { renderWithI18n } from "../test/i18n";
import { WorkflowBudgetRequestBanner } from "./workflow-budget-request-banner";

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/paths")>()),
  useCurrentWorkspace: () => ({ id: "ws-1", slug: "acme" }),
  useWorkspacePaths: () => ({ issueDetail: (id: string) => `/acme/issues/${id}` }),
}));

const navigation: NavigationAdapter = {
  push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/workflows", searchParams: new URLSearchParams(), hash: "",
  getShareableUrl: (path) => `https://app.example${path}`,
};

const request = (over: Partial<OpenDecisionRequest> = {}, target = "wf-1", actionType = "workflow.budget.set"): OpenDecisionRequest => ({
  id: "r1", issue_id: "i1", issue_identifier: "CHE-1", asking_agent_id: "a", asking_agent_name: "Agent",
  ask: "Raise the cap", escalation_class: "budget", created_at: "2026-11-12T00:00:00Z", chat_session_id: "s", message_id: null,
  action: {
    id: "act-1", action_type: actionType, payload_sha256: "abc",
    config_change: {
      field: "workflow.budget", target_id: target, keys: ["max_runs_per_issue"], diff_kind: "json", diff: [],
      after: { max_runs_per_issue: 40 }, before_sha256: "b", after_sha256: "c", sha256: "d",
    },
  },
  ...over,
});

describe("WorkflowBudgetRequestBanner (CHE-5285)", () => {
  const listOpenDecisionRequests = vi.fn();
  beforeEach(() => {
    listOpenDecisionRequests.mockReset();
    setApiInstance({ listOpenDecisionRequests } as unknown as ApiClient);
  });
  afterEach(cleanup);

  const view = () =>
    renderWithI18n(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <NavigationProvider value={navigation}>
          <WorkflowBudgetRequestBanner workflowId="wf-1" />
        </NavigationProvider>
      </QueryClientProvider>,
    );

  it("shows the proposed cap and links to the issue card", async () => {
    listOpenDecisionRequests.mockResolvedValue([request()]);
    view();
    const banner = await screen.findByTestId("workflow-budget-request-banner");
    expect(banner.textContent).toContain("40");
    expect(screen.getByRole("link", { name: /CHE-1/ }).getAttribute("href")).toBe("/acme/issues/i1#issue-decision-banner");
  });

  it("stays hidden for another workflow or another action type", async () => {
    listOpenDecisionRequests.mockResolvedValue([request({}, "wf-other"), request({ id: "r2" }, "wf-1", "issue.run_budget.raise")]);
    view();
    await vi.waitFor(() => expect(listOpenDecisionRequests).toHaveBeenCalled());
    expect(screen.queryByTestId("workflow-budget-request-banner")).toBeNull();
  });
});
