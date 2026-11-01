"use client";

import { useState } from "react";
import { Check, Settings, Workflow } from "@multica/ui/icons";
import { toast } from "sonner";
import { clientErrorMessage } from "@multica/core/api";
import { useFeatureEnabled } from "@multica/core/config";
import { PROJECT_WORKFLOWS_V1_FLAG } from "@multica/core/feature-flags";
import { useWorkspaceId } from "@multica/core/hooks";
import { useIssueStatuses } from "@multica/core/issue-statuses/hooks";
import { useIssueWorkflows, useProjectWithWorkflow, useSetIssueWorkflow } from "@multica/core/issue-workflows";
import { useWorkspacePaths } from "@multica/core/paths";
import { Button } from "@multica/ui/components/ui/button";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@multica/ui/components/ui/dialog";
import { Popover, PopoverContent, PopoverTrigger } from "@multica/ui/components/ui/popover";
import { cn } from "@multica/ui/lib/utils";
import { ActorAvatar } from "../common/actor-avatar";
import { StatusIcon } from "../issues/components/status-icon";
import { useStatusLabel } from "../issues/utils/status-label";
import { useT } from "../i18n";
import { useNavigation } from "../navigation";
import { useWorkflowPermissions } from "./permissions";
import { stepHandlerActor, useStepHandlerLabel } from "./step-handler";
import { WorkflowGraph } from "./workflow-graph";

/** Whether the issue's workflow row has anything to show: a workflow, or a switch for its managers. */
export function useIssueFlowVisible(projectId: string | null, workflowId: string | null | undefined): boolean {
  const wsId = useWorkspaceId();
  const enabled = useFeatureEnabled(PROJECT_WORKFLOWS_V1_FLAG, false);
  const { project, workflow } = useProjectWithWorkflow(wsId, projectId, workflowId);
  const { canManageProject } = useWorkflowPermissions(project);
  return !!workflow || (enabled && canManageProject);
}

/**
 * The workflow an issue follows (MUL-7420), opening a card of its columns and
 * who each hands to. The columns change in the workflow itself; people who
 * manage the project's workflow can also point this one issue at another
 * workflow, or back to the project's (CHE-1986).
 */
export function IssueFlowCard({
  issueId,
  projectId,
  workflowId,
  currentStatus,
}: {
  issueId: string;
  projectId: string | null;
  workflowId: string | null | undefined;
  currentStatus: string;
}) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const navigation = useNavigation();
  const paths = useWorkspacePaths();
  const enabled = useFeatureEnabled(PROJECT_WORKFLOWS_V1_FLAG, false);
  const catalog = useIssueStatuses(wsId);
  const labelOf = useStatusLabel(wsId);
  const { project, workflow } = useProjectWithWorkflow(wsId, projectId, workflowId);
  const { canCreateWorkflow, canManageProject } = useWorkflowPermissions(project);
  const { workflows } = useIssueWorkflows(wsId);
  const setWorkflow = useSetIssueWorkflow();
  const handlerLabel = useStepHandlerLabel(project);
  const [open, setOpen] = useState(false);
  const [graphOpen, setGraphOpen] = useState(false);
  const canSwitch = enabled && canManageProject;
  if (!workflow && !canSwitch) return null;

  const choose = (next: string | null) =>
    setWorkflow.mutate(
      { issueId, workflowId: next },
      {
        onSuccess: () => setOpen(false),
        onError: (err) => toast.error(clientErrorMessage(err) ?? t(($) => $.workflows.detail.override_error)),
      },
    );

  return (
    <>
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger
        render={
          <button
            type="button"
            className="-ml-1 inline-flex min-w-0 items-center gap-1.5 rounded-md px-1 py-0.5 text-caption transition-colors hover:bg-accent data-popup-open:bg-accent"
          >
            <Workflow aria-hidden className="size-3.5 shrink-0 text-muted-foreground" />
            <span className="truncate text-foreground">{workflow?.name ?? t(($) => $.workflows.detail.no_workflow)}</span>
            {workflow && (
              <span className="shrink-0 text-muted-foreground">
                {workflowId ? t(($) => $.workflows.detail.follows_override) : t(($) => $.workflows.detail.follows_project)}
              </span>
            )}
          </button>
        }
      />
      <PopoverContent align="start" className="w-72 p-3">
        {workflow && project && !workflowId && (
          <p className="mb-2 text-caption text-muted-foreground">
            {t(($) => $.workflows.detail.card_title, { workflow: workflow.name, project: project.title })}
          </p>
        )}
        <ol className="space-y-0.5">
          {(workflow?.steps ?? []).map((step) => {
            const label = handlerLabel(step);
            const actor = stepHandlerActor(step, project);
            const current = step.status_key === currentStatus;
            return (
              <li
                key={step.status_key}
                className={cn("flex items-center gap-2 rounded-md px-2 py-1.5", current && "bg-accent")}
                aria-current={current ? "step" : undefined}
              >
                <StatusIcon
                  status={step.status_key}
                  category={catalog.categoryOf(step.status_key)}
                  color={catalog.colorOf(step.status_key)}
                  icon={catalog.iconOf(step.status_key)}
                  className="h-3.5 w-3.5 shrink-0"
                />
                <span className={cn("min-w-0 flex-1 truncate text-body", current && "font-medium")}>
                  {labelOf(step.status_key)}
                </span>
                {label && (
                  <span className="flex min-w-0 items-center gap-1 text-caption text-muted-foreground">
                    {actor && <ActorAvatar actorType={actor.type} actorId={actor.id} size="xs" profileLink={false} />}
                    <span className="truncate">{label}</span>
                  </span>
                )}
              </li>
            );
          })}
        </ol>
        {workflow && (
          <Button
            variant="ghost"
            size="sm"
            className="mt-1 w-full justify-start"
            onClick={() => {
              setOpen(false);
              setGraphOpen(true);
            }}
          >
            <Workflow aria-hidden className="size-3.5" />
            {t(($) => $.workflows.detail.flowchart)}
          </Button>
        )}
        {canSwitch && (
          <div className="-mx-3 mt-2 border-t border-surface-border px-1 pt-1">
            <p className="px-2 py-1 text-caption text-muted-foreground">{t(($) => $.workflows.detail.override_title)}</p>
            {projectId && (
              <Button
                variant="ghost"
                size="sm"
                className="w-full justify-start"
                disabled={setWorkflow.isPending}
                onClick={() => choose(null)}
              >
                {!workflowId ? <Check aria-hidden className="size-3.5" /> : <span aria-hidden className="size-3.5" />}
                {t(($) => $.workflows.detail.override_follow_project)}
              </Button>
            )}
            {workflows.map((w) => (
              <Button
                key={w.id}
                variant="ghost"
                size="sm"
                className="w-full justify-start"
                disabled={setWorkflow.isPending}
                onClick={() => choose(w.id)}
              >
                {workflowId === w.id ? <Check aria-hidden className="size-3.5" /> : <span aria-hidden className="size-3.5" />}
                <span className="truncate">{w.name}</span>
              </Button>
            ))}
          </div>
        )}
        {canCreateWorkflow && enabled && (
          <div className="-mx-3 -mb-3 mt-2 border-t border-surface-border p-1">
            <Button
              variant="ghost"
              size="sm"
              className="w-full justify-start"
              onClick={() => {
                setOpen(false);
                navigation.push(`${paths.settings()}?tab=workflows`);
              }}
            >
              <Settings aria-hidden className="size-3.5" />
              {t(($) => $.workflows.project.manage)}
            </Button>
          </div>
        )}
      </PopoverContent>
    </Popover>
    {workflow && (
      <Dialog open={graphOpen} onOpenChange={setGraphOpen}>
        <DialogContent className="sm:max-w-4xl">
          <DialogHeader>
            <DialogTitle>{workflow.name}</DialogTitle>
          </DialogHeader>
          <WorkflowGraph workflow={workflow} currentStatus={currentStatus} project={project} />
        </DialogContent>
      </Dialog>
    )}
    </>
  );
}
