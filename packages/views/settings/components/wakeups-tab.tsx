"use client";

import { useEffect, useId, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Check } from "@multica/ui/icons";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import { useWorkspaceId } from "@multica/core/hooks";
import { useAuthStore } from "@multica/core/auth";
import { useCurrentWorkspace } from "@multica/core/paths";
import { memberListOptions, workspaceKeys } from "@multica/core/workspace/queries";
import { deriveWakeupsPause } from "@multica/core/workspace/wakeups-pause";
import { useUpdateWorkspaceSystemWakeup, workspaceSystemWakeupsOptions } from "@multica/core/issues";
import type { Workspace, WorkspaceSystemWakeup } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { Switch } from "@multica/ui/components/ui/switch";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { WakeupsPausedBanner } from "../../issues/components/wakeups-paused-banner";
import { useT } from "../../i18n";
import { SettingsCard, SettingsRow, SettingsSection, SettingsTab } from "./settings-layout";

const MAX_INSTRUCTION_BYTES = 4000;

/**
 * Workspace defaults of the platform's wakeup rules. Each issue can change
 * its own rule in its Wakeups section; those issues stop following these
 * defaults.
 */
export function WakeupsTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const { data: rules = [], isError, refetch } = useQuery(workspaceSystemWakeupsOptions(wsId));
  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const currentUser = useAuthStore((s) => s.user);
  const canManage = useMemo(() => {
    const role = members.find((m) => m.user_id === currentUser?.id)?.role;
    return role === "owner" || role === "admin";
  }, [members, currentUser]);
  const childDone = rules.find((rule) => rule.rule === "child_done");
  const paused = deriveWakeupsPause(useCurrentWorkspace()).paused;
  return (
    <SettingsTab title={t(($) => $.page.tabs.wakeups)} description={t(($) => $.wakeups.description)}>
      {isError && (
        <div role="alert" className="flex items-center justify-between gap-3 text-caption text-destructive">
          <span>{t(($) => $.wakeups.load_error)}</span>
          <Button type="button" size="sm" variant="outline" data-testid="wakeups-load-retry" onClick={() => void refetch()}>
            {t(($) => $.wakeups.retry)}
          </Button>
        </div>
      )}
      <PauseSwitch canManage={canManage} />
      {childDone && <ChildDoneDefault rule={childDone} canManage={canManage} paused={paused} />}
      {childDone && !canManage && <p className="text-caption text-muted-foreground">{t(($) => $.wakeups.admin_only)}</p>}
    </SettingsTab>
  );
}

/** The master switch: while on, no wakeup of this workspace fires. */
function PauseSwitch({ canManage }: { canManage: boolean }) {
  const { t } = useT("settings");
  const workspace = useCurrentWorkspace();
  const qc = useQueryClient();
  const id = useId();
  const [saving, setSaving] = useState(false);
  // What the last save did; shown until the next one starts.
  const [saved, setSaved] = useState<"paused" | "resumed" | null>(null);
  const paused = deriveWakeupsPause(workspace).paused;
  const toggle = async (next: boolean) => {
    if (!workspace || saving) return;
    setSaving(true);
    setSaved(null);
    try {
      // Only the switch travels: the server applies it to the settings it reads
      // under the workspace lock and stamps who and when, so a stale snapshot of
      // the whole blob can never overwrite a concurrent change.
      const updated = await api.updateWorkspace(workspace.id, { wakeups_paused: next });
      qc.setQueryData(workspaceKeys.list(), (old: Workspace[] | undefined) =>
        old?.map((ws) => (ws.id === updated.id ? updated : ws)),
      );
      setSaved(next ? "paused" : "resumed");
    } catch {
      toast.error(t(($) => $.wakeups.pause_error));
    } finally {
      setSaving(false);
    }
  };
  return (
    <SettingsSection anchor="pause">
      <SettingsCard>
        <SettingsRow
          label={<label htmlFor={`${id}-paused`}>{t(($) => $.wakeups.pause_label)}</label>}
          description={t(($) => $.wakeups.pause_description)}
        >
          <Switch
            id={`${id}-paused`}
            data-testid="wakeups-pause-switch"
            checked={paused}
            disabled={!canManage || saving}
            aria-busy={saving || undefined}
            onCheckedChange={(next) => void toggle(next)}
          />
        </SettingsRow>
      </SettingsCard>
      {saved && (
        <p
          role="status"
          data-testid="wakeups-pause-status"
          className="flex items-center gap-1.5 rounded-md border border-success/30 bg-success/10 px-3 py-2 text-caption leading-5 text-foreground"
        >
          <Check className="size-3.5 shrink-0 text-success" aria-hidden="true" />
          <span>{saved === "paused" ? t(($) => $.wakeups.pause_saved) : t(($) => $.wakeups.pause_resumed)}</span>
        </p>
      )}
      <WakeupsPausedBanner />
    </SettingsSection>
  );
}

function ChildDoneDefault({ rule, canManage, paused }: { rule: WorkspaceSystemWakeup; canManage: boolean; paused: boolean }) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const update = useUpdateWorkspaceSystemWakeup(wsId);
  const id = useId();
  const [instruction, setInstruction] = useState(rule.instruction);
  const [error, setError] = useState("");
  // A save elsewhere (another tab, another admin) replaces an untouched draft.
  useEffect(() => setInstruction(rule.instruction), [rule.instruction]);
  const dirty = instruction.trim() !== rule.instruction;
  const save = async (input: { enabled?: boolean; instruction?: string }) => {
    try {
      await update.mutateAsync({ rule: rule.rule, ...input });
    } catch {
      toast.error(t(($) => $.wakeups.save_error));
      throw new Error("save failed");
    }
  };
  return (
    <SettingsSection
      anchor="child-done"
      title={t(($) => $.wakeups.child_done_title)}
      description={t(($) => $.wakeups.child_done_description)}
    >
      <SettingsCard>
        <SettingsRow
          label={<label htmlFor={`${id}-enabled`}>{t(($) => $.wakeups.enabled)}</label>}
          description={
            paused
              ? t(($) => $.wakeups.overridden)
              : rule.customized > 0
                ? t(($) => $.wakeups.customized, { count: rule.customized })
                : undefined
          }
        >
          <Switch
            id={`${id}-enabled`}
            data-testid="wakeups-child-done-switch"
            checked={rule.enabled}
            disabled={!canManage || paused || update.isPending}
            aria-busy={update.isPending || undefined}
            onCheckedChange={(enabled) => void save({ enabled }).catch(() => undefined)}
          />
        </SettingsRow>
        <form
          className="space-y-2 px-4 py-3.5"
          onSubmit={(event) => {
            event.preventDefault();
            const next = instruction.trim();
            if (new TextEncoder().encode(next).length > MAX_INSTRUCTION_BYTES) {
              setError(t(($) => $.wakeups.instruction_invalid));
              return;
            }
            void save({ instruction: next }).catch(() => undefined);
          }}
        >
          <label htmlFor={`${id}-instruction`} className="text-body font-medium">
            {t(($) => $.wakeups.instruction)}
          </label>
          <p id={`${id}-hint`} className="text-caption leading-5 text-muted-foreground">
            {t(($) => $.wakeups.instruction_hint)}
          </p>
          <Textarea
            id={`${id}-instruction`}
            data-testid="wakeups-child-done-instruction"
            rows={3}
            value={instruction}
            placeholder={t(($) => $.wakeups.instruction_placeholder)}
            disabled={!canManage || update.isPending}
            aria-describedby={error ? `${id}-hint ${id}-error` : `${id}-hint`}
            aria-invalid={!!error}
            className="resize-y text-base md:text-body"
            onChange={(event) => {
              setInstruction(event.target.value);
              setError("");
            }}
            onKeyDown={(event) => {
              if ((event.metaKey || event.ctrlKey) && event.key === "Enter" && !event.nativeEvent.isComposing) {
                event.preventDefault();
                event.currentTarget.form?.requestSubmit();
              }
            }}
          />
          {error && (
            <p id={`${id}-error`} role="alert" className="text-caption text-destructive">
              {error}
            </p>
          )}
          {rule.builtin_instruction && (
            <div className="rounded-md bg-muted/60 px-3 py-2 text-caption leading-5 text-muted-foreground">
              <p className="font-medium">{t(($) => $.wakeups.builtin)}</p>
              <p className="mt-0.5 whitespace-pre-wrap break-words">{rule.builtin_instruction}</p>
            </div>
          )}
          {canManage && (
            <div className="flex justify-end">
              <Button type="submit" size="sm" disabled={!dirty || update.isPending}>
                {update.isPending ? t(($) => $.wakeups.saving) : t(($) => $.wakeups.save)}
              </Button>
            </div>
          )}
        </form>
      </SettingsCard>
    </SettingsSection>
  );
}
