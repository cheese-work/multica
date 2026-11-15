"use client";

import { useState } from "react";
import { Workflow } from "@multica/ui/icons";
import { useFeatureEnabled } from "@multica/core/config";
import { PROJECT_WORKFLOWS_V1_FLAG } from "@multica/core/feature-flags";
import { useWorkspaceId } from "@multica/core/hooks";
import { useIssueWorkflows, useProjectWithWorkflow } from "@multica/core/issue-workflows";
import { Popover, PopoverContent, PopoverTrigger } from "@multica/ui/components/ui/popover";
import { PillButton } from "../common/pill-button";
import { useT } from "../i18n";
import { useWorkflowPermissions } from "./permissions";
import { WorkflowOverrideOptions } from "./workflow-override-options";

/**
 * The workflow a new issue will follow, for a create dialog: "from project"
 * until someone picks another. Shown only to people who may switch an issue's
 * workflow, as the server refuses anyone else. (CHE-2822)
 */
export function WorkflowChoicePill({
  projectId,
  workflowId,
  onChange,
}: {
  projectId: string | null;
  workflowId: string | null;
  onChange: (workflowId: string | null) => void;
}) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const enabled = useFeatureEnabled(PROJECT_WORKFLOWS_V1_FLAG, false);
  const { project } = useProjectWithWorkflow(wsId, projectId);
  const { canManageProject } = useWorkflowPermissions(project);
  const { workflows } = useIssueWorkflows(wsId);
  const [open, setOpen] = useState(false);
  if (!enabled || !canManageProject) return null;
  const chosen = workflows.find((w) => w.id === workflowId);
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger
        render={
          <PillButton>
            <Workflow aria-hidden className="size-3.5 shrink-0 text-muted-foreground" />
            <span className="truncate">{chosen?.name ?? t(($) => $.workflows.detail.follows_project)}</span>
          </PillButton>
        }
      />
      <PopoverContent align="start" className="w-72 p-1">
        <WorkflowOverrideOptions
          workflowId={workflowId}
          projectId={projectId}
          onChoose={(next) => {
            onChange(next);
            setOpen(false);
          }}
        />
      </PopoverContent>
    </Popover>
  );
}
