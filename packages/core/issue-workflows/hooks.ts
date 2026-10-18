import { useQuery } from "@tanstack/react-query";
import { useCallback, useMemo } from "react";
import { projectListOptions } from "../projects/queries";
import type { IssueWorkflow, Project } from "../types";
import { issueWorkflowListOptions, resolveProjectWorkflow } from "./queries";

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
 * workflow — including while either list is still loading, which renders the
 * Default behavior rather than an empty board.
 */
export function useProjectWithWorkflow(
  wsId: string,
  projectId: string | null | undefined,
): { project: Project | null; workflow: IssueWorkflow | null } {
  const { workflows } = useIssueWorkflows(wsId);
  const { data: projects } = useQuery({
    ...projectListOptions(wsId),
    enabled: Boolean(wsId) && Boolean(projectId),
  });
  return useMemo(() => {
    const project = projectId ? projects?.find((p) => p.id === projectId) ?? null : null;
    return { project, workflow: resolveProjectWorkflow(workflows, project) };
  }, [projectId, projects, workflows]);
}

/** The workflow a project uses, or null for the Default workflow. */
export function useProjectWorkflow(
  wsId: string,
  projectId: string | null | undefined,
): IssueWorkflow | null {
  return useProjectWithWorkflow(wsId, projectId).workflow;
}
