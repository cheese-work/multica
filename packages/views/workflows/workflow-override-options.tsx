"use client";

import { Check } from "@multica/ui/icons";
import { useWorkspaceId } from "@multica/core/hooks";
import { useIssueWorkflows } from "@multica/core/issue-workflows";
import { Button } from "@multica/ui/components/ui/button";
import { useT } from "../i18n";

/**
 * The choice of workflow for one issue: follow its project's, or any workspace
 * workflow (CHE-1986). Shared by the issue's flow card and the create dialogs
 * so the two cannot offer different choices. `projectId` null hides "follow
 * project", which an issue outside a project has nothing to follow.
 */
export function WorkflowOverrideOptions({
  workflowId,
  projectId,
  pending = false,
  onChoose,
}: {
  workflowId: string | null | undefined;
  projectId: string | null | undefined;
  pending?: boolean;
  onChoose: (workflowId: string | null) => void;
}) {
  const { t } = useT("issues");
  const { workflows } = useIssueWorkflows(useWorkspaceId());
  const mark = (selected: boolean) =>
    selected ? <Check aria-hidden className="size-3.5" /> : <span aria-hidden className="size-3.5" />;
  return (
    <>
      <p className="px-2 py-1 text-caption text-muted-foreground">{t(($) => $.workflows.detail.override_title)}</p>
      {projectId && (
        <Button variant="ghost" size="sm" className="w-full justify-start" disabled={pending} onClick={() => onChoose(null)}>
          {mark(!workflowId)}
          {t(($) => $.workflows.detail.override_follow_project)}
        </Button>
      )}
      {workflows.map((w) => (
        <Button
          key={w.id}
          variant="ghost"
          size="sm"
          className="w-full justify-start"
          disabled={pending}
          onClick={() => onChoose(w.id)}
        >
          {mark(workflowId === w.id)}
          <span className="truncate">{w.name}</span>
        </Button>
      ))}
    </>
  );
}
