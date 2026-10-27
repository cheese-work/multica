"use client";

import { useState } from "react";
import { useWorkspaceId } from "@multica/core/hooks";
import { useUpdateWorkspaceJevGovernance } from "@multica/core/workspace/mutations";
import type { GovernanceAuditEntry, WorkspaceJevGovernance } from "@multica/core/types";
import { createSafeId } from "@multica/core/utils";
import { Button } from "@multica/ui/components/ui/button";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { useT } from "../../i18n";
import { diffSettings, restoresRoutingOn } from "./jev-history";
import { useChangeFormat } from "./use-change-format";
import { problemOf } from "./jev-problem";

const REVISION_CONFLICT = "configuration_revision_conflict";

/**
 * Confirms a rollback and shows what it changes first. The write carries the
 * version the user was looking at, so a save that landed in the meantime is a
 * conflict to review, not an overwrite. The history refreshes only from the
 * server's answer to a write that succeeded.
 */
export function JevRollbackDialog({
  config,
  target,
  online,
  reload,
  onClose,
  onDone,
}: {
  config: WorkspaceJevGovernance;
  target: GovernanceAuditEntry | null;
  online: boolean;
  reload: () => Promise<boolean>;
  onClose: () => void;
  onDone: (version: number) => void;
}) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const update = useUpdateWorkspaceJevGovernance(wsId);
  const { value, label } = useChangeFormat();
  const [failure, setFailure] = useState<"conflict" | "failed" | null>(null);
  const [reloading, setReloading] = useState(false);

  const current = config.history.find((entry) => entry.version === config.version);
  // With no saved version yet there is nothing to roll back from.
  const now = current?.effective ?? {};
  const diff = target ? diffSettings(now, target.effective) : null;
  const busy = update.isPending || reloading;

  const close = () => {
    if (busy) return;
    setFailure(null);
    update.reset();
    onClose();
  };
  const confirm = async () => {
    if (!target || restoresRoutingOn(target)) return;
    setFailure(null);
    try {
      await update.mutateAsync({
        expected_version: config.version,
        request_id: createSafeId(),
        rollback_to_version: target.version,
      });
      update.reset();
      onDone(target.version);
    } catch (error) {
      setFailure(problemOf(error) === REVISION_CONFLICT ? "conflict" : "failed");
    }
  };
  const reloadAndReview = async () => {
    setReloading(true);
    const loaded = await reload();
    setReloading(false);
    // A failed reload keeps the dialog and its explanation on screen.
    if (loaded) close();
  };

  return (
    <AlertDialog open={target !== null} onOpenChange={(next) => (next ? undefined : close())}>
      <AlertDialogContent data-testid="jev-rollback-dialog">
        {target && diff ? (
          <>
            <AlertDialogHeader>
              <AlertDialogTitle>
                {t(($) => $.jev_governance.history.dialog_title, { version: target.version })}
              </AlertDialogTitle>
              <AlertDialogDescription>
                {t(($) => $.jev_governance.history.dialog_body, {
                  version: target.version,
                  next: config.version + 1,
                })}
              </AlertDialogDescription>
            </AlertDialogHeader>
            {diff.changes.length > 0 ? (
              <table className="w-full text-left text-caption">
                <thead>
                  <tr className="text-muted-foreground">
                    <th scope="col" className="py-1 pr-3 font-medium">
                      {t(($) => $.jev_governance.history.col_setting)}
                    </th>
                    <th scope="col" className="py-1 pr-3 font-medium">
                      {t(($) => $.jev_governance.history.col_now, { version: config.version })}
                    </th>
                    <th scope="col" className="py-1 font-medium">
                      {t(($) => $.jev_governance.history.col_after, { version: target.version })}
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {diff.changes.map((change) => (
                    <tr key={change.key} className="border-t border-surface-border">
                      <td className="py-1.5 pr-3">{label(change)}</td>
                      <td className="py-1.5 pr-3">
                        {value(change, "before") ?? t(($) => $.jev_governance.history.opaque_value)}
                      </td>
                      <td className="py-1.5 font-medium">
                        {value(change, "after") ?? t(($) => $.jev_governance.history.opaque_value)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ) : (
              <p className="text-caption text-muted-foreground">
                {t(($) => $.jev_governance.history.no_differences)}
              </p>
            )}
            {diff.unchanged > 0 ? (
              <p className="text-caption text-muted-foreground">
                {t(($) => $.jev_governance.history.unchanged, { count: diff.unchanged })}
              </p>
            ) : null}
            {failure ? (
              <div role="alert" className="space-y-1 rounded-lg border border-destructive/30 bg-destructive/5 px-3 py-2">
                <p className="text-body font-medium">
                  {failure === "conflict"
                    ? t(($) => $.jev_governance.history.conflict_title)
                    : t(($) => $.jev_governance.history.failed)}
                </p>
                {failure === "conflict" ? (
                  <p className="text-caption text-muted-foreground">
                    {t(($) => $.jev_governance.history.conflict_body)}
                  </p>
                ) : null}
              </div>
            ) : null}
            <AlertDialogFooter>
              {failure ? (
                <>
                  <AlertDialogCancel disabled={busy}>{t(($) => $.jev_governance.history.close)}</AlertDialogCancel>
                  {failure === "conflict" ? (
                    <Button disabled={reloading || !online} onClick={() => void reloadAndReview()}>
                      {t(($) => $.jev_governance.history.reload)}
                    </Button>
                  ) : null}
                </>
              ) : (
                <>
                  <AlertDialogCancel disabled={busy}>{t(($) => $.jev_governance.cancel)}</AlertDialogCancel>
                  <Button
                    disabled={!online || busy}
                    aria-busy={update.isPending ? true : undefined}
                    onClick={() => void confirm()}
                  >
                    {t(($) => $.jev_governance.history.confirm, { version: target.version })}
                  </Button>
                </>
              )}
            </AlertDialogFooter>
          </>
        ) : null}
      </AlertDialogContent>
    </AlertDialog>
  );
}
