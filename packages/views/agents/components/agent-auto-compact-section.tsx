"use client";

import { useState } from "react";
import { toast } from "sonner";
import type { Agent } from "@multica/core/types";
import { Input } from "@multica/ui/components/ui/input";
import {
  AUTO_COMPACT_MAX_TOKENS_1M,
  AUTO_COMPACT_MAX_TOKENS_200K,
  AUTO_COMPACT_MIN_TOKENS,
  isValidAutoCompactTokens,
} from "@multica/core/workspace/auto-compact";
import { useT } from "../../i18n";

type TokenField = "threshold_tokens_200k" | "threshold_tokens_1m";

function savedTokens(agent: Agent, field: TokenField): number | null {
  const raw = agent.runtime_config?.auto_compact;
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) return null;
  const value = (raw as Record<string, unknown>)[field];
  return typeof value === "number" ? value : null;
}

/**
 * Agent-level override of the workspace auto-compact token thresholds. An
 * empty field inherits the workspace value. Other runtime_config keys and the
 * other auto_compact fields are preserved on save.
 */
export function AgentAutoCompactSection({
  agent,
  canEdit,
  onUpdate,
}: {
  agent: Agent;
  canEdit: boolean;
  onUpdate: (id: string, data: Record<string, unknown>) => Promise<void>;
}) {
  const { t } = useT("agents");
  const { t: ts } = useT("settings");
  const [drafts, setDrafts] = useState<Partial<Record<TokenField, string>>>({});

  const save = async (field: TokenField, text: string) => {
    const { auto_compact, ...rest } = agent.runtime_config ?? {};
    const { [field]: _dropped, ...kept } =
      auto_compact !== null && typeof auto_compact === "object" && !Array.isArray(auto_compact)
        ? (auto_compact as Record<string, unknown>)
        : {};
    // A saved percent would outrank the token limit being saved, so it goes.
    const { threshold_pct: _pct, ...withoutPct } = kept;
    const next = text.trim() === "" ? kept : { ...withoutPct, [field]: Number(text) };
    try {
      await onUpdate(agent.id, {
        runtime_config: Object.keys(next).length ? { ...rest, auto_compact: next } : rest,
      });
      setDrafts(({ [field]: _done, ...remaining }) => remaining);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t(($) => $.auto_compact.save_failed));
    }
  };

  const row = (field: TokenField, max: number, label: string, invalidText: string) => {
    const saved = savedTokens(agent, field);
    const text = drafts[field] ?? (saved === null ? "" : String(saved));
    const invalid =
      drafts[field] !== undefined && text.trim() !== "" && !isValidAutoCompactTokens(Number(text), max);
    return (
      <div className="space-y-1">
        <label className="text-caption font-medium" htmlFor={`agent-auto-compact-${field}`}>
          {label}
        </label>
        <Input
          id={`agent-auto-compact-${field}`}
          type="number"
          inputMode="numeric"
          min={AUTO_COMPACT_MIN_TOKENS}
          max={max}
          step={1}
          aria-label={label}
          aria-invalid={invalid || undefined}
          placeholder={t(($) => $.auto_compact.inherit_placeholder)}
          value={text}
          disabled={!canEdit}
          onChange={(event) => setDrafts((d) => ({ ...d, [field]: event.target.value }))}
          onBlur={() => {
            const draft = drafts[field];
            if (draft === undefined || invalid) return;
            const savedText = saved === null ? "" : String(saved);
            if (draft.trim() === savedText) {
              setDrafts(({ [field]: _same, ...remaining }) => remaining);
              return;
            }
            void save(field, draft);
          }}
        />
        {invalid ? <p className="text-caption text-destructive">{invalidText}</p> : null}
      </div>
    );
  };

  return (
    <section className="space-y-3">
      <div>
        <h3 className="text-body font-medium">{t(($) => $.auto_compact.title)}</h3>
        <p className="text-caption text-muted-foreground">{t(($) => $.auto_compact.hint)}</p>
      </div>
      {row(
        "threshold_tokens_200k",
        AUTO_COMPACT_MAX_TOKENS_200K,
        ts(($) => $.workspace.auto_compact_tokens_200k_label),
        ts(($) => $.workspace.auto_compact_tokens_200k_invalid),
      )}
      {row(
        "threshold_tokens_1m",
        AUTO_COMPACT_MAX_TOKENS_1M,
        ts(($) => $.workspace.auto_compact_tokens_1m_label),
        ts(($) => $.workspace.auto_compact_tokens_1m_invalid),
      )}
    </section>
  );
}
