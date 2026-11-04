"use client";

import { useWorkspaceId } from "@multica/core/hooks";
import { useIssueWorkflows } from "@multica/core/issue-workflows";
import { Workflow } from "@multica/ui/icons";

/** Names the workflow an issue overrides its project's with; nothing when it follows the project. */
export function WorkflowPill({ workflowId }: { workflowId?: string | null }) {
  const { workflows } = useIssueWorkflows(useWorkspaceId());
  const name = workflowId ? workflows.find((w) => w.id === workflowId)?.name : undefined;
  if (!name) return null;
  return (
    <span className="inline-flex max-w-[140px] shrink-0 items-center gap-1 rounded-full bg-muted/60 px-1.5 py-0.5 text-micro text-muted-foreground">
      <Workflow className="size-3 shrink-0" />
      <span className="truncate">{name}</span>
    </span>
  );
}
