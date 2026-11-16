"use client";

import { useState } from "react";
import { toast } from "sonner";
import type { Workspace } from "@multica/core/types";
import { useSetWorkspaceScheduling } from "@multica/core/workspace/mutations";
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
import { useT } from "../../i18n";
import { SettingsRow } from "./settings-layout";

/**
 * Pause / resume claims for every agent in the workspace (CHE-2954). Owner or
 * admin only; for anyone else the row is absent. Resume needs no confirm.
 */
export function WorkspaceSchedulingRow({
  workspace,
  canManage,
}: {
  workspace: Workspace;
  canManage: boolean;
}) {
  const { t } = useT("settings");
  const mutation = useSetWorkspaceScheduling();
  const [confirming, setConfirming] = useState(false);
  if (!canManage) return null;

  const paused = !!workspace.scheduling_paused_at;
  const set = (pause: boolean) =>
    mutation.mutate(
      { id: workspace.id, pause },
      {
        onSuccess: () =>
          toast.success(
            t(($) => (pause ? $.workspace.scheduling_paused_toast : $.workspace.scheduling_resumed_toast)),
          ),
        onError: () => toast.error(t(($) => $.workspace.scheduling_failed_toast)),
        onSettled: () => setConfirming(false),
      },
    );

  return (
    <SettingsRow
      anchor="scheduling"
      label={t(($) => $.workspace.scheduling_label)}
      description={t(($) => $.workspace.scheduling_hint)}
    >
      <Button
        variant="outline"
        size="sm"
        disabled={mutation.isPending}
        onClick={() => (paused ? set(false) : setConfirming(true))}
      >
        {paused
          ? t(($) => $.workspace.scheduling_resume_action)
          : t(($) => $.workspace.scheduling_pause_action)}
      </Button>
      {confirming && (
        <AlertDialog open onOpenChange={(v) => !v && !mutation.isPending && setConfirming(false)}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>{t(($) => $.workspace.scheduling_pause_title)}</AlertDialogTitle>
              <AlertDialogDescription>
                {t(($) => $.workspace.scheduling_pause_description)}
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel disabled={mutation.isPending}>
                {t(($) => $.workspace.scheduling_keep)}
              </AlertDialogCancel>
              <AlertDialogAction
                variant="destructive"
                disabled={mutation.isPending}
                onClick={(e) => {
                  e.preventDefault();
                  set(true);
                }}
              >
                {t(($) => $.workspace.scheduling_confirm)}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      )}
    </SettingsRow>
  );
}
