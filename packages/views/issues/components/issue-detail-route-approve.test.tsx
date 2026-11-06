import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import { renderWithI18n } from "../../test/i18n";
import { NavigationProvider } from "../../navigation";
import { IssueDetailRoute } from "./issue-detail-route";

vi.mock("./issue-detail", async () => {
  const actual = await vi.importActual<typeof import("./issue-detail")>("./issue-detail");
  return { ...actual, IssueDetail: vi.fn(() => null) };
});
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/paths", async () => {
  const actual = await vi.importActual<typeof import("@multica/core/paths")>("@multica/core/paths");
  return {
    ...actual,
    useCurrentWorkspace: () => ({ id: "ws-1", name: "Acme", slug: "acme" }),
    useWorkspacePaths: () => actual.paths.workspace("acme"),
  };
});

const issueId = "cb240efb-154c-42a8-ae92-42b02676feca";
const replace = vi.fn();
const getDecisionAction = vi.fn();

// The approve URL the run-budget request prints: /<slug>/issues/<id>?approve=<action>.
describe("IssueDetailRoute approval deep link", () => {
  let qc: QueryClient;

  beforeEach(() => {
    replace.mockClear();
    getDecisionAction.mockReset().mockResolvedValue({
      id: "act-1", action_type: "issue.run_budget.raise", action_version: 1, params: { new_cap: 9 },
      payload_sha256: "abc", expires_at: "2026-11-07T00:00:00Z", state: "pending",
    });
    setApiInstance({
      getIssue: vi.fn().mockResolvedValue({ id: issueId, identifier: "TRS-134" }),
      getDecisionAction,
      getDecisionActionOutcome: vi.fn().mockResolvedValue({ id: "act-1", state: "pending" }),
    } as unknown as ApiClient);
    qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } });
  });
  afterEach(() => qc.clear());

  function route(search: string, routeId = "TRS-134") {
    return (
      <QueryClientProvider client={qc}>
        <NavigationProvider
          value={{
            push: vi.fn(), replace, back: vi.fn(),
            pathname: `/acme/issues/${routeId}`,
            searchParams: new URLSearchParams(search),
            hash: "",
            getShareableUrl: (p: string) => `https://app.multica.com${p}`,
          }}
        >
          <IssueDetailRoute routeId={routeId} />
        </NavigationProvider>
      </QueryClientProvider>
    );
  }

  it("shows the passkey approval for the action named by ?approve=", async () => {
    renderWithI18n(route("approve=act-1"));
    expect(await screen.findByRole("button", { name: "Approve with passkey" })).toBeTruthy();
    expect(getDecisionAction).toHaveBeenCalledWith("act-1");
    expect(screen.getByText(/9/)).toBeTruthy();
  });

  it("shows nothing without ?approve=", async () => {
    renderWithI18n(route(""));
    await waitFor(() => expect(replace).not.toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
    expect(getDecisionAction).not.toHaveBeenCalled();
  });

  it("keeps ?approve= when it canonicalizes a UUID route", async () => {
    renderWithI18n(route("approve=act-1", issueId));
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/acme/issues/TRS-134?approve=act-1"));
  });

  it("says so when the action cannot be read", async () => {
    getDecisionAction.mockRejectedValue(new Error("forbidden"));
    renderWithI18n(route("approve=act-1"));
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
  });
});
