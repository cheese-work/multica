import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { useWorkspaceId } from "../hooks";
import { issueKeys } from "../issues/queries";
import { projectKeys } from "../projects/queries";
import type {
  IssueWorkflowWriteRequest,
  ListIssueWorkflowsResponse,
  SetProjectWorkflowRequest,
  SetWorkflowHandoffRequest,
} from "../types";
import { issueWorkflowKeys } from "./queries";

/**
 * Workflow mutations (MUL-7420).
 *
 * Like the status catalog, the list refreshes through the
 * `issue_workflow:changed` realtime event; the writer only invalidates on
 * failure, when its own copy is the likely stale one. A project switch or a
 * step removal can move issues between statuses, and those moves arrive as
 * ordinary issue events.
 */

export function useCreateIssueWorkflow() {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return useMutation({
    mutationFn: (data: IssueWorkflowWriteRequest) => api.createIssueWorkflow(data),
    onError: () => {
      qc.invalidateQueries({ queryKey: issueWorkflowKeys.all(wsId) });
    },
  });
}

export function useUpdateIssueWorkflow() {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return useMutation({
    mutationFn: ({ id, ...data }: { id: string } & IssueWorkflowWriteRequest) =>
      api.updateIssueWorkflow(id, data),
    onSuccess: (saved, variables) => {
      // The next save names this version. Waiting for the refetch or the
      // realtime event would leave a quick second save naming the old one and
      // refused as a conflict with the editor's own write. A dry run saved
      // nothing.
      if (variables.dry_run || !saved?.id) return;
      qc.setQueryData<ListIssueWorkflowsResponse>(issueWorkflowKeys.list(wsId), (previous) =>
        previous ? { ...previous, workflows: previous.workflows.map((w) => (w.id === saved.id ? saved : w)) } : previous,
      );
    },
    onError: () => {
      qc.invalidateQueries({ queryKey: issueWorkflowKeys.all(wsId) });
    },
  });
}

export function useDeleteIssueWorkflow() {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return useMutation({
    mutationFn: (id: string) => api.deleteIssueWorkflow(id),
    onError: () => {
      qc.invalidateQueries({ queryKey: issueWorkflowKeys.all(wsId) });
    },
  });
}

/** Sets or clears one issue's workflow override; the issue event refreshes its caches. */
export function useSetIssueWorkflow() {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return useMutation({
    mutationFn: ({ issueId, workflowId }: { issueId: string; workflowId: string | null }) =>
      api.setIssueWorkflow(issueId, workflowId),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: issueKeys.all(wsId) });
    },
  });
}

export function useSetProjectWorkflow() {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return useMutation({
    mutationFn: ({ projectId, ...data }: { projectId: string } & SetProjectWorkflowRequest) =>
      api.setProjectWorkflow(projectId, data),
    onSettled: (_data, _error, { projectId }) => {
      qc.invalidateQueries({ queryKey: projectKeys.list(wsId) });
      qc.invalidateQueries({ queryKey: projectKeys.detail(wsId, projectId) });
      // project_ids on each workflow row changed.
      qc.invalidateQueries({ queryKey: issueWorkflowKeys.all(wsId) });
      // Remapped issues moved columns.
      qc.invalidateQueries({ queryKey: issueKeys.all(wsId) });
    },
  });
}

/**
 * Saves a workflow's handoff for one column, for every project using it — or,
 * with `copyForProjectId`, on a copy only that project uses.
 */
export function useSetWorkflowHandoff() {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return useMutation({
    mutationFn: ({
      workflowId,
      statusKey,
      copyForProjectId,
      ...data
    }: { workflowId: string; statusKey: string; copyForProjectId?: string } & SetWorkflowHandoffRequest) =>
      api.setWorkflowHandoff(workflowId, statusKey, data, copyForProjectId),
    onSuccess: (saved, { workflowId, copyForProjectId }) => {
      if (!saved.id) return;
      if (copyForProjectId) {
        // A new workflow row, and the project now points at it.
        qc.invalidateQueries({ queryKey: issueWorkflowKeys.all(wsId) });
        qc.invalidateQueries({ queryKey: projectKeys.list(wsId) });
        qc.invalidateQueries({ queryKey: projectKeys.detail(wsId, copyForProjectId) });
        return;
      }
      qc.setQueryData<ListIssueWorkflowsResponse>(issueWorkflowKeys.list(wsId), (old) =>
        old && { ...old, workflows: old.workflows.map((w) => (w.id === workflowId ? saved : w)) },
      );
    },
    onError: () => {
      qc.invalidateQueries({ queryKey: issueWorkflowKeys.all(wsId) });
    },
  });
}
