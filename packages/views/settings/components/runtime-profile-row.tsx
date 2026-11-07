"use client";

import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import type { Workspace } from "@multica/core/types";
import {
  HARNESS_PROFILES,
  readHarnessProfile,
  type HarnessProfile,
} from "@multica/core/workspace/harness-profile";
import { workspaceKeys } from "@multica/core/workspace/queries";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { useT } from "../../i18n";
import { SettingsRow } from "./settings-layout";

/**
 * CHE-3706: workspace default Runtime profile (settings.harness_profile).
 * Saves only its own request field, so no other workspace setting is
 * rewritten. An agent can override it on its own page.
 */
export function RuntimeProfileRow({
  workspace,
  canEdit,
}: {
  workspace: Workspace;
  canEdit: boolean;
}) {
  const { t } = useT("settings");
  const qc = useQueryClient();
  const saved = readHarnessProfile(workspace.settings);
  const label = (profile: HarnessProfile) =>
    t(($) => $.workspace[`runtime_profile_${profile}`]);

  async function save(next: HarnessProfile) {
    try {
      const updated = await api.updateWorkspace(workspace.id, { harness_profile: next });
      qc.setQueryData(workspaceKeys.list(), (old: Workspace[] | undefined) =>
        old?.map((ws) => (ws.id === updated.id ? updated : ws)),
      );
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t(($) => $.workspace.toast_save_failed),
      );
    }
  }

  return (
    <SettingsRow
      anchor="runtime-profile"
      label={t(($) => $.workspace.runtime_profile_label)}
      description={t(($) => $.workspace.runtime_profile_hint)}
      size={canEdit ? "select" : undefined}
    >
      {canEdit ? (
        <Select
          items={HARNESS_PROFILES.map((p) => ({ value: p, label: label(p) }))}
          value={saved}
          onValueChange={(next) => next && next !== saved && save(next as HarnessProfile)}
        >
          <SelectTrigger
            size="sm"
            className="w-full"
            aria-label={t(($) => $.workspace.runtime_profile_label)}
          >
            <SelectValue />
          </SelectTrigger>
          <SelectContent align="end">
            {HARNESS_PROFILES.map((p) => (
              <SelectItem key={p} value={p}>
                {label(p)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      ) : (
        <p className="text-body text-foreground">{label(saved)}</p>
      )}
    </SettingsRow>
  );
}
