"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { Trash2 } from "@multica/ui/icons";
import { toast } from "sonner";
import { Button } from "@multica/ui/components/ui/button";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { useQuery } from "@tanstack/react-query";
import type { BatchUpdateIssuesResponse, Issue, UpdateIssueRequest } from "@multica/core/types";
import { useWorkspaceId } from "@multica/core/hooks";
import { commonIssueFields } from "@multica/core/issues/batch";
import {
  NO_HANDOFF_BINDING,
  resolveIssueWorkflow,
  sharedWorkflowStatusKeys,
  stepHandsOff,
  useIssueWorkflows,
  workflowStep,
} from "@multica/core/issue-workflows";
import { projectListOptions } from "@multica/core/projects/queries";
import { useBatchUpdateIssues, useBatchDeleteIssues, useUpdateIssue } from "@multica/core/issues/mutations";
import { useModalStore } from "@multica/core/modals";
import {
  BatchHandoffConfirmDialog,
  type BatchHandoffChoice,
} from "../../workflows/batch-handoff-confirm-dialog";
import { useNotifyCatalogUnresolved } from "../../workflows/use-notify-catalog-unresolved";
import { StatusPicker, PriorityPicker, AssigneePicker } from "./pickers";
import { BatchWorkflowPicker } from "./batch-workflow-picker";
import { useT } from "../../i18n";
import { CollectionBatchBar } from "../../layout/collection-list";
import { useIssueSurfaceActionsOptional } from "../surface/actions-context";
import { useIssueSurfaceSelection } from "../surface/selection-context";
import { useRegisterCommandBarContext } from "../../search/register-context";

export function BatchActionToolbar({
  issues,
  placement = "fixed-bottom",
  clearSelectionOnApply = false,
}: {
  /**
   * The universe of selectable issues at this call site (the same list the
   * rows are rendered from). The toolbar filters it by the active surface
   * selection to reflect the real common status / priority / assignee of the
   * selected issues, mirroring how the skill list filters rows by `selectedIds`.
   */
  issues: Issue[];
  /**
   * "fixed-bottom" — floats at the bottom of the viewport (default; used by
   * full-screen issue lists).
   * "inline" — renders in normal flow so callers can place it adjacent to
   * the selected rows (used inside scrollable sections like sub-issues).
   */
  placement?: "fixed-bottom" | "inline";
  /**
   * Drop the selection once a status / priority / assignee change fully
   * applies. Off for full-screen lists, where the selection stays so several
   * edits can be chained; on for sub-issues, where a batch edit is a one-shot
   * dispatch and a lingering inline toolbar reads as unfinished. A partial or
   * failed write keeps the selection so the user can retry.
   */
  clearSelectionOnApply?: boolean;
}) {
  const { t } = useT("issues");
  const selection = useIssueSurfaceSelection();
  const selectedIds = selection.selectedIds;
  const clear = selection.clear;

  // The authoritative selection is selectedIds ∩ the visible universe. Acting
  // on raw selectedIds while other consumers (Export selected, common fields)
  // intersect lets one "N selected" mean different sets — e.g. a realtime
  // update drops a selected row from the window, batch still mutates it but
  // export omits it. Count, pickers, and every action below share this set.
  const selectedIssues = useMemo(
    () => issues.filter((i) => selectedIds.has(i.id)),
    [issues, selectedIds],
  );
  const count = selectedIssues.length;

  useRegisterCommandBarContext(
    useMemo(
      () => (count > 0 ? { kind: "issues", issues: selectedIssues, clearSelection: clear } : null),
      [count, selectedIssues, clear],
    ),
  );

  // Reflect the real shared value of the selected issues in each picker; fall
  // back to an empty (no-checkmark) state when the selection is mixed, instead
  // of asserting a hardcoded default.
  const common = useMemo(
    () => commonIssueFields(selectedIssues),
    [selectedIssues],
  );

  // A selection inside one project offers that project's workflow statuses;
  // one spanning projects offers the statuses every selected issue's
  // workflow allows. (MUL-7420)
  const sharedProjectId = useMemo(() => {
    const first = selectedIssues[0]?.project_id ?? null;
    return first && selectedIssues.every((i) => i.project_id === first) ? first : null;
  }, [selectedIssues]);
  const wsId = useWorkspaceId();
  const { workflows, isLoaded: workflowsLoaded, retry: retryWorkflows } = useIssueWorkflows(wsId);
  // The projects tell which workflow each selected issue follows, so a selection
  // with any project issue needs them, whether or not it shares one project.
  const { data: projects, refetch: refetchProjects } = useQuery({
    ...projectListOptions(wsId),
    enabled: count > 0 && selectedIssues.some((i) => i.project_id !== null),
  });
  const notifyCatalogUnresolved = useNotifyCatalogUnresolved();
  // One override shared by the whole selection is listed by the picker itself,
  // handoffs included, instead of being cut down from the project's list.
  // (CHE-2822)
  const sharedWorkflowId = useMemo(() => {
    const first = selectedIssues[0]?.workflow_id ?? null;
    return first && selectedIssues.every((i) => i.workflow_id === first) ? first : null;
  }, [selectedIssues]);
  // Differing effective workflows (overrides, or an override beside its
  // project's) list the whole catalog narrowed to the keys they share; the
  // project's own list would hide a shared status it lacks.
  const sameWorkflow = !!sharedWorkflowId || !selectedIssues.some((i) => i.workflow_id);
  const allowedStatuses = useMemo(
    () =>
      sharedWorkflowId || (sharedProjectId && !selectedIssues.some((i) => i.workflow_id))
        ? null
        : sharedWorkflowStatusKeys(workflows, projects, selectedIssues),
    [sharedWorkflowId, sharedProjectId, workflows, projects, selectedIssues],
  );

  const [statusOpen, setStatusOpen] = useState(false);
  const [priorityOpen, setPriorityOpen] = useState(false);
  const [assigneeOpen, setAssigneeOpen] = useState(false);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [pendingHandoff, setPendingHandoff] = useState<{
    updates: Partial<UpdateIssueRequest>;
    ids: string[];
    handingOff: Issue[];
  } | null>(null);
  const surfaceActions = useIssueSurfaceActionsOptional();
  const batchUpdate = useBatchUpdateIssues();
  const updateIssue = useUpdateIssue();
  const batchDelete = useBatchDeleteIssues();
  const openModal = useModalStore((s) => s.open);
  const loading =
    surfaceActions?.isPending ?? (batchUpdate.isPending || batchDelete.isPending);
  const ids = selectedIssues.map((issue) => issue.id);

  useEffect(() => {
    if (count > 0) return;
    setStatusOpen(false);
    setPriorityOpen(false);
    setAssigneeOpen(false);
    setDeleteOpen(false);
  }, [count]);

  const applyBatch = (batchIds: string[], updates: Partial<UpdateIssueRequest>) =>
    surfaceActions
      ? surfaceActions.batchUpdate(batchIds, updates)
      : batchUpdate.mutateAsync({ ids: batchIds, updates });

  // A batch status write binds one result for every issue it carries: none of
  // them hands off. The ones that would are confirmed and written one by one,
  // so a handler that appeared since the catalog was read (or an issue the
  // catalog did not cover) is skipped by the server, never applied unconfirmed.
  const withNoHandoffBinding = (updates: Partial<UpdateIssueRequest>): Partial<UpdateIssueRequest> =>
    updates.status ? { ...updates, handoff_preview: NO_HANDOFF_BINDING } : updates;

  // The re-run of a partial write must read the catalog as it is by then, not as
  // it was when the toast was built.
  const rerun = useRef<(updates: Partial<UpdateIssueRequest>, targets: Issue[]) => void>(() => {});

  // Success, or a partial result that names the issues left as they were and
  // offers to try those again.
  const reportOutcome = (
    requested: string[],
    writtenIds: string[] | null,
    written: number,
    updates: Partial<UpdateIssueRequest>,
  ) => {
    if (written >= requested.length) {
      if (clearSelectionOnApply) clear();
      toast.success(t(($) => $.batch.update_success, { count: requested.length }));
      return;
    }
    const left = writtenIds ? requested.filter((id) => !writtenIds.includes(id)) : [];
    if (left.length === 0) {
      toast.warning(t(($) => $.batch.update_partial, { updated: written, count: requested.length }));
      return;
    }
    const names = left.map((id) => issues.find((i) => i.id === id)?.identifier ?? id).join(", ");
    toast.warning(t(($) => $.batch.update_partial_named, { updated: written, count: requested.length, issues: names }), {
      action: {
        label: t(($) => $.batch.retry_not_updated),
        onClick: () => rerun.current(updates, issues.filter((i) => left.includes(i.id))),
      },
    });
  };

  const reportFailure = (err: unknown) =>
    toast.error(
      err instanceof Error && err.message
        ? err.message
        : t(($) => $.batch.update_failed),
    );

  const handleBatchUpdate = async (updates: Partial<UpdateIssueRequest>, targetIds: string[] = ids) => {
    try {
      const result: BatchUpdateIssuesResponse = await applyBatch(targetIds, updates);
      reportOutcome(targetIds, result.updated_ids ?? null, result.updated, updates);
    } catch (err) {
      reportFailure(err);
    }
  };

  // A batch status change applies directly (MUL-4155) except where it enters a
  // workflow step that hands an issue off (MUL-7420): that needs the same
  // confirmation a single status change gets, and the write then carries what
  // was confirmed. Issues that hand nothing off still move in the one batch
  // call, only after the confirmation. done/cancelled can never start a run,
  // and a backlog → active promotion starts its run the same way a single-issue
  // status change or the CLI does, without an extra confirmation step (product
  // decision on MUL-4155). Agent/squad assignment still confirms via
  // handleBatchAssignee.
  const handleBatchStatus = (updates: Partial<UpdateIssueRequest>, targets: Issue[] = selectedIssues) => {
    const status = updates.status;
    if (!status) return;
    const targetIds = targets.map((i) => i.id);
    const inProjects = targets.filter((i) => (i.project_id || i.workflow_id) && i.status !== status);
    const resolved = inProjects.map((issue) => ({
      issue,
      ...resolveIssueWorkflow(workflowsLoaded ? workflows : undefined, projects, issue.project_id, issue.workflow_id),
    }));
    // A workflow that is merely unloaded, failed to load, or loaded without this
    // project's workflow reads as "no handoff"; the server would still apply the
    // real one unconfirmed.
    if (resolved.some((r) => !r.resolved)) {
      notifyCatalogUnresolved(() => {
        retryWorkflows();
        void refetchProjects();
      });
      return;
    }
    const handingOff = resolved
      .filter((r) => stepHandsOff(workflowStep(r.workflow, status)))
      .map((r) => r.issue);
    if (handingOff.length > 0) {
      setPendingHandoff({ updates, ids: targetIds, handingOff });
      return;
    }
    void handleBatchUpdate(withNoHandoffBinding(updates), targetIds);
  };
  rerun.current = (updates, targets) =>
    updates.status ? handleBatchStatus(updates, targets) : void handleBatchUpdate(updates, targets.map((i) => i.id));

  const confirmHandoff = async (choices: Map<string, BatchHandoffChoice>) => {
    const pending = pendingHandoff;
    setPendingHandoff(null);
    if (!pending) return;
    const handed = new Set(pending.handingOff.map((i) => i.id));
    const rest = pending.ids.filter((id) => !handed.has(id));
    const writes = [
      ...pending.handingOff.map((i) => {
        const choice = choices.get(i.id)!;
        return updateIssue
          .mutateAsync({
            id: i.id,
            ...pending.updates,
            ...(choice.stopPreviousRuns ? { stop_previous_assignee_runs: true } : {}),
            handoff_preview: choice.preview,
          })
          .then(() => [i.id]);
      }),
      ...(rest.length > 0
        ? [applyBatch(rest, withNoHandoffBinding(pending.updates)).then((r) => r.updated_ids ?? (r.updated >= rest.length ? rest : []))]
        : []),
    ];
    const settled = await Promise.allSettled(writes);
    const writtenIds = settled.flatMap((r) => (r.status === "fulfilled" ? r.value : []));
    const failure = settled.find((r): r is PromiseRejectedResult => r.status === "rejected");
    if (failure && writtenIds.length === 0) reportFailure(failure.reason);
    else reportOutcome(pending.ids, writtenIds, writtenIds.length, pending.updates);
  };

  const handleBatchAssignee = (updates: Partial<UpdateIssueRequest>) => {
    if ((updates.assignee_type === "agent" || updates.assignee_type === "squad") && updates.assignee_id) {
      // Backlog never starts a run on assign (parking lot), so if every selected
      // issue is in backlog the confirm modal would only render an empty "won't
      // start" box — apply directly, matching handleBatchStatus's backlog short-
      // circuit. A mixed selection still routes through the modal: the non-backlog
      // issues will trigger and need confirmation.
      // Only the fixed Backlog key parks work; custom Unstarted does not.
      const allBacklog = selectedIssues.every((i) => i.status === "backlog");
      if (!allBacklog) {
        openModal("issue-run-confirm", {
          issueIds: ids,
          mode: "assign",
          assigneeType: updates.assignee_type,
          assigneeId: updates.assignee_id,
          onApplied: clearSelectionOnApply ? clear : undefined,
        });
        return;
      }
    }
    void handleBatchUpdate(updates);
  };

  const handleBatchDelete = async () => {
    try {
      if (surfaceActions) {
        await surfaceActions.batchDelete(ids);
      } else {
        await batchDelete.mutateAsync(ids);
      }
      clear();
      toast.success(t(($) => $.batch.delete_success, { count }));
    } catch (err) {
      toast.error(
        err instanceof Error && err.message
          ? err.message
          : t(($) => $.batch.delete_failed),
      );
    } finally {
      setDeleteOpen(false);
    }
  };

  return (
    <>
      <CollectionBatchBar
        count={count}
        onClear={clear}
        placement={placement === "fixed-bottom" ? "fixed" : "inline"}
        label={t(($) => $.batch.selected, { count })}
      >
        {/* Status */}
        <StatusPicker
          status={common.status}
          // No shared project spans projects (or none): not the Default-only
          // list of a single issue outside a project. (MUL-8121)
          projectId={sameWorkflow ? (sharedProjectId ?? undefined) : undefined}
          workflowId={sharedWorkflowId}
          allowedStatuses={allowedStatuses}
          onUpdate={handleBatchStatus}
          open={statusOpen}
          onOpenChange={setStatusOpen}
          triggerRender={<Button variant="ghost" size="sm" disabled={loading} />}
          trigger={t(($) => $.batch.status)}
          align="center"
        />

        {/* Priority */}
        <PriorityPicker
          priority={common.priority}
          onUpdate={handleBatchUpdate}
          open={priorityOpen}
          onOpenChange={setPriorityOpen}
          triggerRender={<Button variant="ghost" size="sm" disabled={loading} />}
          trigger={t(($) => $.batch.priority)}
          align="center"
        />

        {/* Assignee */}
        <AssigneePicker
          assigneeType={common.assignee?.type ?? null}
          assigneeId={common.assignee?.id ?? null}
          mixed={common.assignee === null}
          onUpdate={handleBatchAssignee}
          open={assigneeOpen}
          onOpenChange={setAssigneeOpen}
          triggerRender={<Button variant="ghost" size="sm" disabled={loading} />}
          trigger={t(($) => $.batch.assignee)}
          align="center"
        />

        {/* Workflow */}
        <BatchWorkflowPicker
          issues={selectedIssues}
          onApplied={clearSelectionOnApply ? clear : undefined}
        />

        {/* Delete */}
        <Button
          variant="ghost"
          size="sm"
          disabled={loading}
          onClick={() => setDeleteOpen(true)}
          className="text-destructive hover:text-destructive"
        >
          <Trash2 className="size-3.5 mr-1" />
          {t(($) => $.batch.delete)}
        </Button>
      </CollectionBatchBar>

      {pendingHandoff && (
        <BatchHandoffConfirmDialog
          issues={pendingHandoff.handingOff}
          status={pendingHandoff.updates.status ?? ""}
          total={pendingHandoff.ids.length}
          onCancel={() => setPendingHandoff(null)}
          onConfirm={(choices) => void confirmHandoff(choices)}
        />
      )}

      {count > 0 && <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(($) => $.batch.delete_dialog_title, { count })}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.batch.delete_dialog_desc, { count })}
              <span className="mt-2 block text-caption text-muted-foreground">
                {t(($) => $.batch.delete_dialog_warning)}
              </span>
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t(($) => $.batch.cancel)}</AlertDialogCancel>
            <AlertDialogAction
              onClick={handleBatchDelete}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              {t(($) => $.batch.delete)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>}
    </>
  );
}
