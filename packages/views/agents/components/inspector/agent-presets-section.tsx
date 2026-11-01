"use client";

import { useQuery } from "@tanstack/react-query";
import { agentPresetsOptions, presetSettingsSummary } from "@multica/core/agents";
import { Badge } from "@multica/ui/components/ui/badge";
import { SettingsCard, SettingsRow, SettingsSection } from "../../../settings/components/settings-layout";
import { useT } from "../../../i18n";

/** Read-only list of the agent's presets. A stale preset cannot run for the agent as it is now. */
export function AgentPresetsSection({ wsId, agentId }: { wsId: string; agentId: string }) {
  const { t } = useT("agents");
  const { data: presets = [] } = useQuery(agentPresetsOptions(wsId, agentId));
  if (presets.length === 0) return null;

  // The server's reason set is open: a reason this client does not know is shown as sent.
  const reasonLabel = (reason: string) => {
    switch (reason) {
      case "unknown_preset": return t(($) => $.inspector.preset_stale_reason.unknown_preset);
      case "ambiguous": return t(($) => $.inspector.preset_stale_reason.ambiguous);
      case "context_args": return t(($) => $.inspector.preset_stale_reason.context_args);
      case "thinking_level": return t(($) => $.inspector.preset_stale_reason.thinking_level);
      case "service_tier": return t(($) => $.inspector.preset_stale_reason.service_tier);
      case "model": return t(($) => $.inspector.preset_stale_reason.model);
      case "lab_lock": return t(($) => $.inspector.preset_stale_reason.lab_lock);
      default: return reason;
    }
  };

  return (
    <SettingsSection title={t(($) => $.inspector.section_presets)}>
      <SettingsCard>
        {presets.map((p) => (
          <SettingsRow
            key={p.name}
            label={
              <span className="flex flex-wrap items-center gap-2">
                {p.name}
                {p.is_default ? <Badge variant="secondary">{t(($) => $.inspector.preset_default)}</Badge> : null}
                {p.stale ? <Badge variant="destructive">{t(($) => $.inspector.preset_stale)}</Badge> : null}
              </span>
            }
            description={p.stale ? reasonLabel(p.stale) : undefined}
          >
            <span className="text-caption text-muted-foreground">{presetSettingsSummary(p)}</span>
          </SettingsRow>
        ))}
      </SettingsCard>
    </SettingsSection>
  );
}
