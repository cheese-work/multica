"use client";

import { useEffect, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { ArrowRight } from "@multica/ui/icons";
import { useWorkspaceId } from "@multica/core/hooks";
import { workflowHandoffPreviewOptions } from "@multica/core/issue-workflows";
import type { HandoffPreviewBinding, WorkflowHandoffPreview } from "@multica/core/types";
import { useActorName } from "@multica/core/workspace/hooks";
import { Button } from "@multica/ui/components/ui/button";
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Spinner } from "@multica/ui/components/ui/spinner";
import { ActorAvatar } from "../common/actor-avatar";
import { useStatusLabel } from "../issues/utils/status-label";
import { useT } from "../i18n";
import { previewBinding } from "./handoff-confirm-dialog";

/** What the person confirmed for one issue: whether to stop the previous run, and the state they saw. */
export interface BatchHandoffChoice {
  stopPreviousRuns: boolean;
  preview: HandoffPreviewBinding;
}

type Candidate = { id: string; identifier: string };

/**
 * Confirms a batch status change that may hand issues off (MUL-7420), like
 * `WorkflowHandoffConfirmDialog` does for one: who each issue goes to, and one
 * choice for stopping the previous assignees' runs. `issues` are the ones the
 * status can hand off; mount the dialog only while one is pending. `total` is
 * the whole selection, which the person confirms as one move.
 *
 * Every preview is fetched fresh when the dialog opens. Nothing is confirmed
 * while one is pending or failed, and a failure offers a retry. When no preview
 * finds a handoff after all, the move applies without asking.
 */
export function BatchHandoffConfirmDialog({
  issues,
  status,
  total,
  onCancel,
  onConfirm,
}: {
  issues: Candidate[];
  status: string;
  total: number;
  onCancel: () => void;
  onConfirm: (choices: Map<string, BatchHandoffChoice>) => void;
}) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const queryClient = useQueryClient();
  const labelOf = useStatusLabel(wsId);
  const { getActorName } = useActorName();
  const [stop, setStop] = useState(true);
  const [attempt, setAttempt] = useState(0);
  const [loaded, setLoaded] = useState<{
    key: string;
    previews: Map<string, WorkflowHandoffPreview> | null;
    failed: boolean;
  }>({ key: "", previews: null, failed: false });
  const key = `${issues.map((i) => i.id).join(",")}|${status}|${attempt}`;

  useEffect(() => {
    let live = true;
    setLoaded({ key, previews: null, failed: false });
    Promise.all(issues.map((i) => queryClient.fetchQuery(workflowHandoffPreviewOptions(wsId, i.id, status))))
      .then((list) => {
        if (live) setLoaded({ key, previews: new Map(issues.map((i, n) => [i.id, list[n]!])), failed: false });
      })
      .catch(() => {
        if (live) setLoaded({ key, previews: null, failed: true });
      });
    return () => {
      live = false;
    };
    // `issues` is read for the fetch; `key` already names its contents.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, queryClient, wsId, status]);

  const current = loaded.key === key;
  const previews = current ? loaded.previews : null;
  const handoffs = previews ? issues.filter((i) => previews.get(i.id)?.handoff) : [];
  const failed = current && loaded.failed;

  const choices = (stopRuns: boolean) =>
    new Map<string, BatchHandoffChoice>(
      issues.map((i) => {
        const p = previews!.get(i.id)!;
        const sameHandler = p.previous_assignee_id === p.handler_id && p.previous_assignee_type === p.handler_type;
        return [i.id, { stopPreviousRuns: stopRuns && p.handoff && p.previous_runs.length > 0 && !sameHandler, preview: previewBinding(p) }];
      }),
    );

  // Nothing to confirm: apply as an ordinary status change, bound to the results.
  useEffect(() => {
    if (previews && handoffs.length === 0) onConfirm(choices(false));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [previews]);

  const hasRuns = handoffs.some((i) => (previews?.get(i.id)?.previous_runs.length ?? 0) > 0);

  return (
    <Dialog open={!(previews && handoffs.length === 0)} onOpenChange={(v) => !v && onCancel()}>
      <DialogContent className="sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>{t(($) => $.workflows.confirm.batch_title, { count: total, status: labelOf(status) })}</DialogTitle>
        </DialogHeader>

        {previews ? (
          <div className="space-y-4">
            <p className="text-body">{t(($) => $.workflows.confirm.batch_summary, { count: handoffs.length })}</p>
            <ul className="space-y-1.5 text-body">
              {handoffs.map((i) => {
                const p = previews.get(i.id)!;
                return (
                  <li key={i.id} className="flex items-center gap-2">
                    <span className="text-muted-foreground">{i.identifier}</span>
                    <ArrowRight aria-hidden className="size-3 shrink-0" />
                    {p.handler_type && p.handler_id && (
                      <span className="inline-flex items-center gap-1.5 font-medium">
                        <ActorAvatar actorType={p.handler_type} actorId={p.handler_id} size="xs" profileLink={false} />
                        {getActorName(p.handler_type, p.handler_id)}
                      </span>
                    )}
                  </li>
                );
              })}
            </ul>
            {hasRuns && (
              <label className="flex cursor-pointer items-center gap-2 text-body">
                <Checkbox checked={stop} onCheckedChange={(v) => setStop(v === true)} />
                {t(($) => $.workflows.confirm.batch_stop_label)}
              </label>
            )}
          </div>
        ) : failed ? (
          <div role="alert" className="flex flex-wrap items-center gap-3">
            <p className="text-body text-destructive">{t(($) => $.workflows.confirm.batch_error)}</p>
            <Button variant="outline" size="sm" onClick={() => setAttempt((n) => n + 1)}>
              {t(($) => $.workflows.editor.retry)}
            </Button>
          </div>
        ) : (
          <div className="flex justify-center py-6">
            <Spinner className="size-4" />
          </div>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={onCancel}>
            {t(($) => $.workflows.confirm.cancel)}
          </Button>
          <Button disabled={!previews} onClick={() => onConfirm(choices(stop))}>
            {t(($) => $.workflows.confirm.batch_confirm, { count: total })}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
