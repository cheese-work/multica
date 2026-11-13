/**
 * Workspace workflow list (CHE-2822). Changes only when an admin edits a
 * workflow, so a long `staleTime` keeps it off the render path.
 */
import { queryOptions } from "@tanstack/react-query";
import { api } from "@/data/api";

export const issueWorkflowKeys = {
  list: (wsId: string | null) => ["issue-workflows", wsId, "list"] as const,
};

export const issueWorkflowListOptions = (wsId: string | null) =>
  queryOptions({
    queryKey: issueWorkflowKeys.list(wsId),
    queryFn: async ({ signal }) => (await api.listIssueWorkflows({ signal })).workflows,
    enabled: !!wsId,
    staleTime: 5 * 60_000,
  });
