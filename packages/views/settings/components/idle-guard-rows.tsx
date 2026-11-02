"use client";

import { useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import type { Workspace } from "@multica/core/types";
import { workspaceKeys } from "@multica/core/workspace/queries";
import { Input } from "@multica/ui/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { useT } from "../../i18n";
import { SettingsRow } from "./settings-layout";

const MODES = ["off", "shadow", "act"] as const;
type IdleGuardMode = (typeof MODES)[number];
const DEFAULT_AFTER_HOURS = 4;
const MAX_AFTER_HOURS = 72;

const isValidHours = (value: number) =>
  Number.isInteger(value) && value >= 1 && value <= MAX_AFTER_HOURS;

/** settings.idle_started_guard is untyped JSON: a malformed field reads as its default. */
function readIdleGuard(settings: Record<string, unknown> | undefined) {
  const raw = settings?.idle_started_guard;
  const guard = raw !== null && typeof raw === "object" ? (raw as Record<string, unknown>) : {};
  const mode = MODES.find((m) => m === guard.mode) ?? "off";
  const hours = typeof guard.after_hours === "number" && isValidHours(guard.after_hours)
    ? guard.after_hours
    : DEFAULT_AFTER_HOURS;
  return { mode, hours };
}

/**
 * CHE-3296: the idle-issue guard (CHE-2609) mode and threshold. Each save
 * sends only the changed field in its own request field, so no other
 * workspace setting is rewritten. Saves run one at a time, in the order the
 * owner made them. Owner only, as on the server.
 */
export function IdleGuardRows({ workspace, canEdit }: { workspace: Workspace; canEdit: boolean }) {
  const { t } = useT("settings");
  const qc = useQueryClient();
  const saved = readIdleGuard(workspace.settings);
  const [hoursDraft, setHoursDraft] = useState<string | null>(null);
  const hoursText = hoursDraft ?? String(saved.hours);
  const hoursInvalid = hoursDraft !== null && (hoursText.trim() === "" || !isValidHours(Number(hoursText)));
  const queue = useRef<Promise<unknown>>(Promise.resolve());
  const modeLabel = (mode: IdleGuardMode) => t(($) => $.workspace[`idle_guard_${mode}`]);

  function save(next: { mode?: IdleGuardMode; after_hours?: number }, onSaved?: () => void) {
    const run = async () => {
      try {
        const updated = await api.updateWorkspace(workspace.id, { idle_started_guard: next });
        qc.setQueryData(workspaceKeys.list(), (old: Workspace[] | undefined) =>
          old?.map((ws) => (ws.id === updated.id ? updated : ws)),
        );
        onSaved?.();
      } catch (error) {
        toast.error(error instanceof Error ? error.message : t(($) => $.workspace.toast_save_failed));
      }
    };
    queue.current = queue.current.then(run);
  }

  function saveHours() {
    if (hoursDraft === null || hoursInvalid) return;
    const hours = Number(hoursText);
    if (hours === saved.hours) return setHoursDraft(null);
    save({ after_hours: hours }, () => setHoursDraft(null));
  }

  return (
    <>
      <SettingsRow
        anchor="idle-guard"
        label={t(($) => $.workspace.idle_guard_label)}
        description={t(($) => $.workspace.idle_guard_hint)}
        size={canEdit ? "select" : undefined}
      >
        {canEdit ? (
          <Select
            items={MODES.map((mode) => ({ value: mode, label: modeLabel(mode) }))}
            value={saved.mode}
            onValueChange={(next) => next && next !== saved.mode && save({ mode: next as IdleGuardMode })}
          >
            <SelectTrigger size="sm" className="w-full" aria-label={t(($) => $.workspace.idle_guard_label)}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent align="end">
              {MODES.map((mode) => (
                <SelectItem key={mode} value={mode}>{modeLabel(mode)}</SelectItem>
              ))}
            </SelectContent>
          </Select>
        ) : (
          <p className="text-body text-foreground">
            {t(($) => $.workspace.idle_guard_readonly, { mode: modeLabel(saved.mode), hours: saved.hours })}
          </p>
        )}
      </SettingsRow>

      {canEdit ? (
        <SettingsRow label={t(($) => $.workspace.idle_guard_hours_label)} size="code">
          <Input
            type="number"
            inputMode="numeric"
            min={1}
            max={MAX_AFTER_HOURS}
            step={1}
            name="workspace-idle-guard-hours"
            aria-label={t(($) => $.workspace.idle_guard_hours_label)}
            aria-invalid={hoursInvalid || undefined}
            value={hoursText}
            onChange={(event) => setHoursDraft(event.target.value)}
            onBlur={saveHours}
          />
          {hoursInvalid ? (
            <p className="mt-1 text-caption text-destructive">
              {t(($) => $.workspace.idle_guard_hours_invalid)}
            </p>
          ) : null}
        </SettingsRow>
      ) : null}
    </>
  );
}
