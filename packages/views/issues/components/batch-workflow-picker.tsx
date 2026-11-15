"use client";

import { useState } from "react";
import { toast } from "sonner";
import { useQuery } from "@tanstack/react-query";
import { useFeatureEnabled } from "@multica/core/config";
import { PROJECT_WORKFLOWS_V1_FLAG } from "@multica/core/feature-flags";
import { useWorkspaceId } from "@multica/core/hooks";
import { useIssueWorkflows, useSetIssueWorkflow } from "@multica/core/issue-workflows";
import { projectListOptions } from "@multica/core/projects/queries";
import type { Issue } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { useT } from "../../i18n";
import { useWorkflowPermissions } from "../../workflows/permissions";
import { PickerItem, PropertyPicker } from "./pickers/property-picker";

/**
 * Points the selected issues at one workflow, or back to their projects'
 * (CHE-2822). Offered as the server admits it: workspace admins, or a lead of
 * every selected issue's project, behind the project-workflows flag. The
 * server judges each issue (its status in the workflow, a live task) and the
 * ones it refuses are named.
 */
export function BatchWorkflowPicker({
  issues,
  onApplied,
}: {
  issues: Issue[];
  /** Called once every selected issue was switched. */
  onApplied?: () => void;
}) {
  const enabled = useFeatureEnabled(PROJECT_WORKFLOWS_V1_FLAG, false);
  return enabled ? <PermittedWorkflowMenu issues={issues} onApplied={onApplied} /> : null;
}

function PermittedWorkflowMenu({ issues, onApplied }: { issues: Issue[]; onApplied?: () => void }) {
  const wsId = useWorkspaceId();
  const { isAdmin, isProjectLead } = useWorkflowPermissions();
  const { data: projects } = useQuery(projectListOptions(wsId));
  const allowed =
    isAdmin ||
    issues.every((i) => isProjectLead(i.project_id ? projects?.find((p) => p.id === i.project_id) : null));
  return allowed ? <WorkflowMenu issues={issues} onApplied={onApplied} /> : null;
}

function WorkflowMenu({ issues, onApplied }: { issues: Issue[]; onApplied?: () => void }) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const { workflows } = useIssueWorkflows(wsId);
  const setWorkflow = useSetIssueWorkflow();
  const [open, setOpen] = useState(false);

  // An issue without a project has none to return to.
  const canFollowProject = issues.every((i) => i.project_id);
  const shared = issues.every((i) => (i.workflow_id ?? null) === (issues[0]?.workflow_id ?? null))
    ? (issues[0]?.workflow_id ?? null)
    : undefined;

  const choose = async (workflowId: string | null) => {
    setOpen(false);
    const settled = await Promise.allSettled(
      issues.map((i) => setWorkflow.mutateAsync({ issueId: i.id, workflowId })),
    );
    const failed = issues.filter((_, n) => settled[n]!.status === "rejected");
    const updated = issues.length - failed.length;
    if (failed.length === 0) {
      onApplied?.();
      toast.success(t(($) => $.batch.workflow_success, { count: updated }));
    } else if (updated === 0) {
      const reason = (settled[0] as PromiseRejectedResult).reason;
      toast.error(reason instanceof Error && reason.message ? reason.message : t(($) => $.workflows.detail.override_error));
    } else {
      toast.warning(
        t(($) => $.batch.workflow_partial, {
          updated,
          count: issues.length,
          issues: failed.map((i) => i.identifier).join(", "),
        }),
      );
    }
  };

  return (
    <PropertyPicker
      open={open}
      onOpenChange={setOpen}
      width="w-56"
      align="center"
      triggerRender={<Button variant="ghost" size="sm" disabled={setWorkflow.isPending} />}
      trigger={t(($) => $.batch.workflow)}
    >
      {canFollowProject && (
        <PickerItem emptyValue selected={shared === null} onClick={() => void choose(null)}>
          <span className="truncate">{t(($) => $.workflows.detail.override_follow_project)}</span>
        </PickerItem>
      )}
      {workflows.map((w) => (
        <PickerItem key={w.id} selected={shared === w.id} onClick={() => void choose(w.id)}>
          <span className="truncate">{w.name}</span>
        </PickerItem>
      ))}
    </PropertyPicker>
  );
}
