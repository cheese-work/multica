"use client";

import { useCallback, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { History } from "@multica/ui/icons";
import { Button } from "@multica/ui/components/ui/button";
import { isBuiltInIssueStatus } from "@multica/core/issue-statuses";
import { useIssueStatuses } from "@multica/core/issue-statuses/hooks";
import { useProjectWithWorkflow } from "@multica/core/issue-workflows";
import { issueStatusHistoryOptions } from "@multica/core/issues/queries";
import { useT } from "../../i18n";
import { useStatusLabel } from "../utils/status-label";
import { StatusHistoryDialog } from "./status-history-dialog";

/**
 * Sidebar entry for the status history. The timeline is fetched only once the
 * dialog opens, so a closed issue page pays nothing for it.
 */
export function StatusHistoryButton({
  issueId,
  wsId,
  projectId,
  workflowId,
}: {
  issueId: string;
  wsId: string;
  projectId: string | null;
  workflowId: string | null | undefined;
}) {
  const { t } = useT("issues");
  const [open, setOpen] = useState(false);
  const { data } = useQuery({ ...issueStatusHistoryOptions(issueId), enabled: open });
  const { entryOf, isPending: catalogPending } = useIssueStatuses(wsId);
  const labelOf = useStatusLabel(wsId);
  const { project, workflow } = useProjectWithWorkflow(wsId, projectId, workflowId);
  // Removed = a custom key the loaded catalog no longer holds. Until the
  // catalog lands every custom key would look removed, so say nothing then.
  const resolveStatus = useCallback(
    (key: string) => ({
      label: labelOf(key),
      removed: !catalogPending && !isBuiltInIssueStatus(key) && entryOf(key) === undefined,
    }),
    [labelOf, entryOf, catalogPending],
  );

  return (
    <div>
      <Button
        type="button"
        variant="ghost"
        size="sm"
        className="w-full justify-start gap-1 font-medium text-muted-foreground"
        onClick={() => setOpen(true)}
      >
        <History className="size-3.5" aria-hidden />
        {t(($) => $.status_history.open)}
      </Button>
      <StatusHistoryDialog
        open={open}
        onOpenChange={setOpen}
        entries={data?.entries ?? []}
        truncated={data?.truncated ?? false}
        resolveStatus={resolveStatus}
        workflow={workflow}
        project={project}
      />
    </div>
  );
}
