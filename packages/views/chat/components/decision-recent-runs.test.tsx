/**
 * @vitest-environment jsdom
 *
 * "Recent runs" on a run-budget card (CHE-5285): the issue's last 5 runs,
 * newest first, so the approver sees what the budget was spent on.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import { renderWithI18n } from "../../test/i18n";
import { DecisionRecentRuns } from "./decision-recent-runs";

const run = (n: number, status: string) => ({
  id: `t${n}`, status, created_at: `2026-11-12T0${n}:00:00Z`, error: null,
});

describe("DecisionRecentRuns (CHE-5285)", () => {
  const listTasksByIssue = vi.fn();
  beforeEach(() => {
    listTasksByIssue.mockReset();
    setApiInstance({ listTasksByIssue } as unknown as ApiClient);
  });
  afterEach(cleanup);

  const view = () =>
    renderWithI18n(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <DecisionRecentRuns issueId="i1" />
      </QueryClientProvider>,
    );

  it("lists the last 5 runs, newest first, with their status", async () => {
    listTasksByIssue.mockResolvedValue([1, 2, 3, 4, 5, 6, 7].map((n) => run(n, n === 7 ? "failed" : "completed")));
    view();
    const items = await screen.findAllByRole("listitem");
    expect(items).toHaveLength(5);
    expect(items[0]!.textContent).toContain("Failed");
    expect(items[0]!.textContent).toContain("07:00 ICT".replace("07", "14"));
    expect(items[4]!.textContent).toContain("Completed");
  });

  it("renders nothing when the issue has no runs", async () => {
    listTasksByIssue.mockResolvedValue([]);
    view();
    await vi.waitFor(() => expect(listTasksByIssue).toHaveBeenCalled());
    expect(screen.queryByTestId("decision-recent-runs")).toBeNull();
  });
});
