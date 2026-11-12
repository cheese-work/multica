import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import { parseRunBudget } from "@multica/core/run-budget";
import { renderWithI18n } from "../../test/i18n";
import { RunBudgetMeter } from "./run-budget-meter";

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/auth/webauthn", () => ({ signPasskeyChallenge: vi.fn().mockResolvedValue({ credential: { id: "k" } }) }));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (select: (s: { user: { id: string } }) => unknown) => select({ user: { id: "user-1" } }),
}));

const issueId = "cb240efb-154c-42a8-ae92-42b02676feca";

const snapshot = (over: Record<string, unknown> = {}) =>
  parseRunBudget({
    budgeted: true, runs_used: 8, runs_left: 32, runs_limit: 40, state: "ok", window_id: "w1",
    window_started_at: "2026-11-12T00:00:00Z", blocking: [], can_raise: true,
    raise_preview: { add_runs: 15, balance_after: 47, limit_after: 55, max_allowed_add: 60 },
    ...over,
  });

describe("RunBudgetMeter (CHE-2861)", () => {
  let qc: QueryClient;
  const getIssueRunBudget = vi.fn();
  const requestRunBudgetAction = vi.fn();
  const listMembers = vi.fn();
  const getDecisionAction = vi.fn();
  const listOpenDecisionRequests = vi.fn();
  const getDecisionActionOutcome = vi.fn();
  const challengeDecisionAction = vi.fn();
  const approveDecisionAction = vi.fn();

  beforeEach(() => {
    sessionStorage.clear();
    getIssueRunBudget.mockReset().mockResolvedValue(snapshot());
    requestRunBudgetAction.mockReset().mockResolvedValue({ requests: [{ request_id: "r1", action_id: "act-1" }] });
    listMembers.mockReset().mockResolvedValue([{ user_id: "user-1", role: "member" }]);
    listOpenDecisionRequests.mockReset().mockResolvedValue([]);
    getDecisionAction.mockReset().mockResolvedValue({
      id: "act-1", action_type: "issue.run_budget.raise", action_version: 1,
      params: { issue_id: issueId, window_id: "w1", new_cap: 55 }, payload_sha256: "abc", state: "pending",
    });
    getDecisionActionOutcome.mockReset().mockResolvedValue({ id: "act-1", state: "pending" });
    challengeDecisionAction.mockReset().mockResolvedValue({ challenge_id: "ch", options: { publicKey: {} }, expires_at: "t" });
    approveDecisionAction.mockReset().mockResolvedValue({ id: "act-1", state: "executed", result: { outcome: "cap_applied" } });
    setApiInstance({
      getIssueRunBudget, requestRunBudgetAction, listMembers, getDecisionAction, listOpenDecisionRequests,
      getDecisionActionOutcome, challengeDecisionAction, approveDecisionAction,
    } as unknown as ApiClient);
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  });
  afterEach(() => qc.clear());

  const view = () =>
    renderWithI18n(
      <QueryClientProvider client={qc}>
        <RunBudgetMeter issueId={issueId} />
      </QueryClientProvider>,
    );

  describe("open request link (CHE-5285)", () => {
    it("links to the open request on this issue and holds the buttons", async () => {
      listOpenDecisionRequests.mockResolvedValue([{ id: "req-1", issue_id: issueId, action: { action_type: "issue.run_budget.raise" } }, { id: "req-2", issue_id: "other", action: { action_type: "issue.run_budget.raise" } }]);
      view();
      const link = await screen.findByRole("link", { name: "Open request below" });
      expect(link.getAttribute("href")).toBe("#issue-decision-banner");
      expect(screen.queryByRole("button", { name: "Top up" })).toBeNull();
    });

    it("keeps the buttons when the open card on this issue is not a budget card", async () => {
      listOpenDecisionRequests.mockResolvedValue([{ id: "req-3", issue_id: issueId }, { id: "req-4", issue_id: issueId, action: { action_type: "agent.env.patch" } }]);
      view();
      expect(await screen.findByRole("button", { name: "Top up" })).toBeTruthy();
      expect(screen.getByRole("button", { name: "Raise" })).toBeTruthy();
      expect(screen.queryByRole("link", { name: "Open request below" })).toBeNull();
    });

    it("keeps the buttons when no request is open on this issue", async () => {
      listOpenDecisionRequests.mockResolvedValue([{ id: "req-2", issue_id: "other" }]);
      view();
      expect(await screen.findByRole("button", { name: "Top up" })).toBeTruthy();
      expect(screen.queryByRole("link", { name: "Open request below" })).toBeNull();
    });
  });

  describe("states", () => {
    it("shows used, limit and runs left", async () => {
      view();
      expect(await screen.findByText("Runs 8 / 40")).toBeTruthy();
      expect(screen.getByText("32 left")).toBeTruthy();
      expect(screen.queryByRole("alert")).toBeNull();
    });

    it("warns at 80 % used", async () => {
      getIssueRunBudget.mockResolvedValue(snapshot({ runs_used: 32, runs_left: 8, state: "near" }));
      view();
      expect(await screen.findByText("Runs 32 / 40")).toBeTruthy();
      expect(screen.getByText("80 % used")).toBeTruthy();
    });

    it("says the budget is used up when exhausted, also when over", async () => {
      getIssueRunBudget.mockResolvedValue(snapshot({ runs_used: 40, runs_left: 0, state: "at_limit" }));
      const first = view();
      expect(await screen.findByText("Run budget used up")).toBeTruthy();
      first.unmount();
      getIssueRunBudget.mockResolvedValue(snapshot({ runs_used: 42, runs_left: -2, state: "over" }));
      view();
      expect(await screen.findByText("Run budget used up")).toBeTruthy();
    });

    it.each([
      ["ok", { runs_used: 20, runs_left: 5, runs_limit: 25 }, "text-foreground"],
      ["near", { runs_used: 20, runs_left: 5, runs_limit: 25 }, "text-warning"],
      ["at_limit", { runs_used: 25, runs_left: 0, runs_limit: 25 }, "text-destructive"],
      ["over", { runs_used: 27, runs_left: -2, runs_limit: 25 }, "text-destructive"],
    ])("colours the runs line for %s", async (state, counts, cls) => {
      getIssueRunBudget.mockResolvedValue(snapshot({ ...counts, state }));
      view();
      const line = await screen.findByText(`Runs ${counts.runs_used} / 25`);
      expect(line.className).toContain(cls);
    });

    it("renders nothing for an unbudgeted issue and for a malformed response", async () => {
      getIssueRunBudget.mockResolvedValue(parseRunBudget({ budgeted: false }));
      const { container } = view();
      await waitFor(() => expect(getIssueRunBudget).toHaveBeenCalled());
      expect(container.textContent).toBe("");
    });

    it("hides Raise when the server offers no raise, keeps Top-up", async () => {
      getIssueRunBudget.mockResolvedValue(snapshot({ can_raise: false, raise_preview: undefined }));
      view();
      await screen.findByText("Runs 8 / 40");
      expect(screen.queryByRole("button", { name: "Raise" })).toBeNull();
      expect(screen.getByRole("button", { name: "Top up" })).toBeTruthy();
    });
  });

  describe("request", () => {
    it("Raise asks for the previewed cap in the window the person saw", async () => {
      view();
      await userEvent.click(await screen.findByRole("button", { name: "Raise" }));
      expect(requestRunBudgetAction).toHaveBeenCalledWith(issueId, { kind: "raise", new_cap: 55, window_id: "w1" });
      expect(await screen.findByText("Asked the workspace owner to approve.")).toBeTruthy();
      expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
    });

    it("Top-up sends no cap", async () => {
      view();
      await userEvent.click(await screen.findByRole("button", { name: "Top up" }));
      expect(requestRunBudgetAction).toHaveBeenCalledWith(issueId, { kind: "top_up", window_id: "w1" });
    });

    it("sends nothing while the window id is unknown", async () => {
      getIssueRunBudget.mockResolvedValue(snapshot({ window_id: undefined }));
      view();
      const raise = (await screen.findByRole("button", { name: "Raise" })) as HTMLButtonElement;
      expect(raise.disabled).toBe(true);
      expect((screen.getByRole("button", { name: "Top up" }) as HTMLButtonElement).disabled).toBe(true);
    });

    it("shows the failure and lets the person ask again", async () => {
      requestRunBudgetAction.mockRejectedValueOnce(new Error("boom"));
      view();
      await userEvent.click(await screen.findByRole("button", { name: "Raise" }));
      expect((await screen.findByRole("alert")).textContent).toContain("The request did not go through");
      await userEvent.click(screen.getByRole("button", { name: "Raise" }));
      expect(await screen.findByText("Asked the workspace owner to approve.")).toBeTruthy();
      expect(requestRunBudgetAction).toHaveBeenCalledTimes(2);
    });
  });

  describe("inline approval", () => {
    it("offers the passkey approval in place when the caller is an owner", async () => {
      listMembers.mockResolvedValue([{ user_id: "user-1", role: "owner" }]);
      view();
      await userEvent.click(await screen.findByRole("button", { name: "Raise" }));
      expect(await screen.findByRole("button", { name: "Approve with passkey" })).toBeTruthy();
      expect(getDecisionAction).toHaveBeenCalledWith("act-1");
      expect(screen.queryByText("Asked the workspace owner to approve.")).toBeNull();
    });
  });

  describe("refresh", () => {
    it("reads the budget again once the inline approval is decided", async () => {
      listMembers.mockResolvedValue([{ user_id: "user-1", role: "owner" }]);
      getIssueRunBudget.mockResolvedValueOnce(snapshot({ runs_used: 40, runs_left: 0, state: "at_limit" }));
      getIssueRunBudget.mockResolvedValue(snapshot({ runs_used: 40, runs_left: 15, runs_limit: 55, state: "ok" }));
      view();
      expect(await screen.findByText("Run budget used up")).toBeTruthy();
      await userEvent.click(await screen.findByRole("button", { name: "Raise" }));
      await userEvent.click(await screen.findByRole("button", { name: "Approve with passkey" }));
      expect(await screen.findByText("Runs 40 / 55")).toBeTruthy();
      expect(screen.queryByText("Run budget used up")).toBeNull();
      expect(getIssueRunBudget).toHaveBeenCalledTimes(2);
    });

    it("drops the asked note when the budget moves to a new window", async () => {
      view();
      await userEvent.click(await screen.findByRole("button", { name: "Top up" }));
      expect(await screen.findByText("Asked the workspace owner to approve.")).toBeTruthy();
      getIssueRunBudget.mockResolvedValue(snapshot({ window_id: "w2", runs_used: 0, runs_left: 40 }));
      await qc.invalidateQueries();
      await waitFor(() => expect(screen.queryByText("Asked the workspace owner to approve.")).toBeNull());
    });

    it("does not tell an owner to wait before the member list has arrived", async () => {
      let resolveMembers: (v: unknown) => void = () => {};
      listMembers.mockReturnValue(new Promise((r) => { resolveMembers = r; }));
      view();
      await userEvent.click(await screen.findByRole("button", { name: "Raise" }));
      expect(screen.queryByText("Asked the workspace owner to approve.")).toBeNull();
      resolveMembers([{ user_id: "user-1", role: "owner" }]);
      expect(await screen.findByRole("button", { name: "Approve with passkey" })).toBeTruthy();
    });
  });
});
