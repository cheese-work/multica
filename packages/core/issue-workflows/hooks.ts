import { useQuery } from "@tanstack/react-query";
import { useCallback, useMemo } from "react";
import { projectListOptions } from "../projects/queries";
import type { IssueWorkflow } from "../types";
import { type IssueWorkflowResolution, defaultWorkflow, issueWorkflowListOptions, resolveIssueWorkflow } from "./queries";

const EMPTY: IssueWorkflow[] = [];

/**
 * The workspace's workflows, in name order. Empty until loaded. `isError` is
 * true only while there is no data to show, so a failed background refetch
 * never replaces a list already on screen. `retry` refetches.
 */
export function useIssueWorkflows(wsId: string): {
  workflows: IssueWorkflow[];
  isLoaded: boolean;
  isError: boolean;
  retry: () => void;
} {
  const { data, isError, refetch } = useQuery({ ...issueWorkflowListOptions(wsId), enabled: Boolean(wsId) });
  const retry = useCallback(() => {
    void refetch();
  }, [refetch]);
  return useMemo(
    () => ({ workflows: data ?? EMPTY, isLoaded: data !== undefined, isError: isError && data === undefined, retry }),
    [data, isError, retry],
  );
}

/**
 * A project and the workflow it uses. `workflow` is null for the Default
 * workflow, and also while either list is still loading or has failed: a board
 * must keep rendering. `resolved` tells the two apart (see
 * `resolveIssueWorkflow`); a consequential write (a status change that can
 * hand the issue off) must wait for it, because a null workflow that is merely
 * unresolved looks exactly like "no handoff" and would skip the confirmation.
 * `retry` reloads both lists.
 *
 * `defaultWorkflow` is the workspace Default itself, null until loaded or on a
 * server without workflows. An issue outside any project offers its statuses.
 * (MUL-8121)
 */
export function useProjectWithWorkflow(
  wsId: string,
  projectId: string | null | undefined,
  workflowId?: string | null,
): IssueWorkflowResolution & {
  /** The workspace Default workflow, once loaded. */
  defaultWorkflow: IssueWorkflow | null;
  /** A list is still on its way; an unresolved answer may yet resolve by itself. */
  loading: boolean;
  retry: () => void;
} {
  const { workflows, isLoaded: workflowsLoaded, isError: workflowsFailed, retry: retryWorkflows } = useIssueWorkflows(wsId);
  const {
    data: projects,
    isPending: projectsPending,
    refetch: refetchProjects,
  } = useQuery({
    ...projectListOptions(wsId),
    enabled: Boolean(wsId) && Boolean(projectId),
  });
  const retry = useCallback(() => {
    retryWorkflows();
    void refetchProjects();
  }, [retryWorkflows, refetchProjects]);
  return useMemo(
    () => ({
      ...resolveIssueWorkflow(workflowsLoaded ? workflows : undefined, projects, projectId, workflowId),
      defaultWorkflow: defaultWorkflow(workflowsLoaded ? workflows : undefined),
      loading: Boolean(projectId || workflowId) && ((!workflowsLoaded && !workflowsFailed) || (projectsPending && projects === undefined)),
      retry,
    }),
    [projectId, workflowId, projects, projectsPending, workflows, workflowsLoaded, workflowsFailed, retry],
  );
}

/** The workflow a project uses, or null for the Default workflow. */
export function useProjectWorkflow(
  wsId: string,
  projectId: string | null | undefined,
): IssueWorkflow | null {
  return useProjectWithWorkflow(wsId, projectId).workflow;
}
