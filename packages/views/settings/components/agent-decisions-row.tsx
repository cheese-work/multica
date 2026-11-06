"use client";

import { useState } from "react";
import { toast } from "sonner";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { Switch } from "@multica/ui/components/ui/switch";
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

// CHE-3249: owner-only (the caller gates it). The CLI and the automatic stop
// write this setting without a workspace update event, so the value is read
// from the brief route on every entry and never from the workspace list cache.
export function AgentDecisionsRow({ wsId }: { wsId: string }) {
  const { t } = useT("settings");
  const qc = useQueryClient();
  const queryKey = ["agent-decisions", "setting", wsId];
  const { data: setting, isFetching, isError } = useQuery({
    queryKey,
    queryFn: () => api.getAgentDecisionSetting(),
    staleTime: 0,
    refetchOnMount: "always",
  });
  // The cached value may predate an external change or an automatic stop, so
  // writes wait for a read that is current and did not fail.
  const writable = !!setting && !isFetching && !isError;
  const [saving, setSaving] = useState(false);
  const [confirmStopReason, setConfirmStopReason] = useState<string | null>(null);

  const save = async (enabled: boolean) => {
    setSaving(true);
    try {
      await api.setAgentDecisionsEnabled(enabled);
      await qc.invalidateQueries({ queryKey });
    } catch (error) {
      toast.error(
        error instanceof Error
          ? error.message
          : t(($) => $.workspace.toast_save_failed),
      );
    } finally {
      setSaving(false);
    }
  };

  // Turning on clears the recorded stop, so show its reason first.
  const onToggle = (enabled: boolean) => {
    if (enabled && setting?.stopped) {
      setConfirmStopReason(setting.stopped.reason);
      return;
    }
    void save(enabled);
  };

  return (
    <>
      <SettingsRow
        anchor="agent-decisions"
        label={t(($) => $.workspace.agent_decisions_label)}
        description={t(($) => $.workspace.agent_decisions_hint)}
      >
        <Switch
          checked={setting?.enabled ?? false}
          disabled={!writable || saving}
          onCheckedChange={onToggle}
          aria-label={t(($) => $.workspace.agent_decisions_label)}
        />
      </SettingsRow>
      <AlertDialog
        open={confirmStopReason !== null}
        onOpenChange={(open) => {
          if (!open) setConfirmStopReason(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(($) => $.workspace.agent_decisions_stopped_title)}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.workspace.agent_decisions_stopped_description, {
                reason: confirmStopReason ?? "",
              })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t(($) => $.workspace.confirm_cancel)}</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                setConfirmStopReason(null);
                void save(true);
              }}
            >
              {t(($) => $.workspace.agent_decisions_stopped_confirm)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
