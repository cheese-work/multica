"use client";

import { useState } from "react";
import { toast } from "sonner";
import { Input } from "@multica/ui/components/ui/input";
import { isValidAutoCompactTokens } from "@multica/core/workspace/auto-compact";
import { useT } from "../../i18n";
import { SettingsRow } from "./settings-layout";
import { useAutoSave } from "./use-auto-save";

interface AutoCompactTokensRowProps {
  label: string;
  invalidText: string;
  name: string;
  saved: number;
  min: number;
  max: number;
  disabled: boolean;
  onSave: (next: number) => Promise<void>;
}

/** One auto-compact token threshold: draft until blur or pause, invalid never sent. */
export function AutoCompactTokensRow({
  label,
  invalidText,
  name,
  saved,
  min,
  max,
  disabled,
  onSave,
}: AutoCompactTokensRowProps) {
  const { t } = useT("settings");
  const [draft, setDraft] = useState<string | null>(null);
  const text = draft ?? String(saved);
  const value = Number(text);
  const invalid =
    draft !== null && (text.trim() === "" || !isValidAutoCompactTokens(value, max));
  const autoSave = useAutoSave({
    value: invalid ? saved : value,
    savedValue: saved,
    onSave,
    onSuccess: () => setDraft(null),
    onError: (error) =>
      toast.error(
        error instanceof Error
          ? error.message
          : t(($) => $.workspace.toast_save_failed),
      ),
    enabled: !disabled,
    isEqual: (left, right) => left === right,
  });

  return (
    <SettingsRow label={label} size="code">
      <Input
        type="number"
        inputMode="numeric"
        min={min}
        max={max}
        step={1}
        name={name}
        aria-label={label}
        aria-invalid={invalid || undefined}
        value={text}
        onChange={(event) => setDraft(event.target.value)}
        onBlur={autoSave.flush}
        disabled={disabled}
      />
      {invalid ? (
        <p className="mt-1 text-caption text-destructive">{invalidText}</p>
      ) : null}
    </SettingsRow>
  );
}
