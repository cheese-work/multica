"use client";

import { presetSettingsSummary } from "@multica/core/agents";
import type { TaskPresetRun } from "@multica/core/types";
import { useT } from "../../i18n";

// Same grid as the run-info popover's rows (RunDetailRow).
function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="grid grid-cols-[4.5rem_minmax(0,1fr)] items-start gap-3 py-0.5">
      <span className="text-muted-foreground">{label}</span>
      <span className="min-w-0 select-text break-all text-foreground">{value}</span>
    </div>
  );
}

/** What the run asked for (claimed) next to what the daemon reported it applied. */
export function RunPresetDetails({ run }: { run: TaskPresetRun }) {
  const { t } = useT("agents");
  const settings = (s: TaskPresetRun["claimed"]) =>
    presetSettingsSummary(s) || t(($) => $.transcript.details_preset_agent_defaults);
  return (
    <>
      <div className="my-2 h-px bg-border" />
      <Row label={t(($) => $.transcript.details_preset)} value={`${run.name} · ${run.source}`} />
      {run.skips.map((skip) => (
        <Row
          key={`${skip.level}:${skip.name}`}
          label={t(($) => $.transcript.details_preset_skipped)}
          value={`${skip.level}: ${skip.name} (${skip.reason})`}
        />
      ))}
      <Row label={t(($) => $.transcript.details_preset_claimed)} value={settings(run.claimed)} />
      <Row
        label={t(($) => $.transcript.details_preset_applied)}
        value={run.applied ? settings(run.applied) : t(($) => $.transcript.details_preset_not_reported)}
      />
    </>
  );
}
