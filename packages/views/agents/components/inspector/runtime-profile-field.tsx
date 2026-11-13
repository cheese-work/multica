"use client";

import {
  HARNESS_PROFILES,
  type HarnessProfile,
} from "@multica/core/workspace/harness-profile";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { useT } from "../../../i18n";

const INHERIT = "inherit";

/**
 * CHE-3706: per-agent override of the workspace Runtime profile
 * (runtime_config.harness_profile). `null` means inherit the workspace default.
 */
export function RuntimeProfileField({
  value,
  canEdit,
  onChange,
}: {
  value: unknown;
  canEdit: boolean;
  onChange: (next: HarnessProfile | null) => void;
}) {
  const { t } = useT("settings");
  const current: HarnessProfile | typeof INHERIT =
    value === "personal" || value === "managed" ? value : INHERIT;
  const label = (v: HarnessProfile | typeof INHERIT) =>
    v === INHERIT
      ? t(($) => $.workspace.runtime_profile_inherit)
      : t(($) => $.workspace[`runtime_profile_${v}`]);
  const options = [INHERIT, ...HARNESS_PROFILES] as const;

  if (!canEdit) return <p className="text-body text-foreground">{label(current)}</p>;
  return (
    <Select
      items={options.map((o) => ({ value: o, label: label(o) }))}
      value={current}
      onValueChange={(next) =>
        next &&
        next !== current &&
        onChange(next === INHERIT ? null : (next as HarnessProfile))
      }
    >
      <SelectTrigger
        size="sm"
        className="w-full"
        aria-label={t(($) => $.workspace.runtime_profile_label)}
      >
        <SelectValue />
      </SelectTrigger>
      <SelectContent align="end">
        {options.map((o) => (
          <SelectItem key={o} value={o}>
            {label(o)}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}
