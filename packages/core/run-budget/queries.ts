import { queryOptions, useQuery } from "@tanstack/react-query";
import { api } from "../api";

export const runBudgetKeys = {
  all: (wsId: string) => ["run-budget", wsId] as const,
  issue: (wsId: string, issueId: string) => [...runBudgetKeys.all(wsId), issueId] as const,
};

export function runBudgetOptions(wsId: string, issueId: string) {
  return queryOptions({
    queryKey: runBudgetKeys.issue(wsId, issueId),
    queryFn: () => api.getIssueRunBudget(issueId),
  });
}

export function useRunBudget(wsId: string, issueId: string) {
  return useQuery({ ...runBudgetOptions(wsId, issueId), enabled: Boolean(wsId && issueId) });
}
