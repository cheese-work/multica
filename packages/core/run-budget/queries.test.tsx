// @vitest-environment jsdom
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, renderHook, waitFor } from "@testing-library/react";
import { api } from "../api";
import { UNBUDGETED } from "./schema";
import { runBudgetKeys, runBudgetOptions, useRunBudget } from "./queries";

vi.mock("../api", () => ({ api: { getIssueRunBudget: vi.fn() } }));
afterEach(() => { cleanup(); vi.clearAllMocks(); });

const wrapperFor = (client: QueryClient) =>
  function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  };
const newClient = () => new QueryClient({ defaultOptions: { queries: { retry: false } } });

describe("run-budget query (CHE-2851)", () => {
  it("keys by workspace and issue so a workspace switch cannot reuse the meter", () => {
    expect(runBudgetKeys.issue("ws-1", "i-1")).toEqual(["run-budget", "ws-1", "i-1"]);
    expect(runBudgetKeys.issue("ws-2", "i-1")).not.toEqual(runBudgetKeys.issue("ws-1", "i-1"));
    expect(runBudgetOptions("ws-1", "i-1").queryKey).toEqual(["run-budget", "ws-1", "i-1"]);
  });

  it("loads the meter through the api client", async () => {
    vi.mocked(api.getIssueRunBudget).mockResolvedValue({ ...UNBUDGETED, budgeted: true, runsLeft: 3 });
    const { result } = renderHook(() => useRunBudget("ws-1", "i-1"), { wrapper: wrapperFor(newClient()) });
    await waitFor(() => expect(result.current.data?.runsLeft).toBe(3));
    expect(api.getIssueRunBudget).toHaveBeenCalledWith("i-1");
  });

  it("goes stale and polls, so a page left open follows runs that use the budget", () => {
    const { staleTime, refetchInterval } = runBudgetOptions("ws-1", "i-1");
    expect(staleTime).toBeLessThanOrEqual(60_000);
    expect(refetchInterval).toBeLessThanOrEqual(60_000);
  });

  it("does not fetch without a workspace or issue id", () => {
    renderHook(() => useRunBudget("", "i-1"), { wrapper: wrapperFor(newClient()) });
    renderHook(() => useRunBudget("ws-1", ""), { wrapper: wrapperFor(newClient()) });
    expect(api.getIssueRunBudget).not.toHaveBeenCalled();
  });
});
