"use client";

import { useMemo } from "react";
import { Check } from "@multica/ui/icons";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../i18n";
import { chipKey, SCOPE_VIEWS, type ChatChip, type ChipCandidate } from "./chat-chips";
import { useActorDirectory } from "./use-actor-directory";

/** Every chip Tab can add, in display order: the scope chips, then agents. Labels match the DM list chips. */
export function useChipCandidates(): ChipCandidate[] {
  const { t } = useT("chat");
  const { agents } = useActorDirectory(true);
  return useMemo(
    () => [
      ...SCOPE_VIEWS.map((view) => ({
        chip: { type: "scope", view } as ChatChip,
        label: t(($) => $.list.filters[view]),
      })),
      ...agents.map(({ agent }) => ({
        chip: { type: "agent", id: agent.id, name: agent.name } as ChatChip,
        label: agent.name,
      })),
    ],
    [agents, t],
  );
}

/**
 * Filter chips under the palette input. The scope chips always show; an agent
 * chip shows while it is on. Same type = OR, different type = AND, Tất cả is
 * exclusive (see chat-chips.ts). The status line is the screen-reader summary.
 */
export function ChatChipBar({
  chips,
  candidates,
  onToggle,
}: {
  chips: readonly ChatChip[];
  candidates: readonly ChipCandidate[];
  onToggle: (chip: ChatChip) => void;
}) {
  const { t } = useT("search");
  const { t: tChat } = useT("chat");
  const on = new Set(chips.map(chipKey));
  const visible = [
    ...candidates.filter(({ chip }) => chip.type === "scope"),
    ...chips.filter((c) => c.type === "agent").map((chip) => ({ chip, label: chip.type === "agent" ? chip.name : "" })),
  ].map(({ chip, label }) => ({
    chip,
    label: chip.type === "agent" ? t(($) => $.chips.agent, { name: label }) : label,
  }));
  const names = visible.filter(({ chip }) => on.has(chipKey(chip))).map(({ label }) => label);

  return (
    <div role="group" aria-label={tChat(($) => $.list.filters.label)} className="flex flex-wrap gap-1.5 border-b px-4 py-2.5">
      {visible.map(({ chip, label }) => {
        const selected = on.has(chipKey(chip));
        return (
          // ui-pattern-ref: allow toggle pill with aria-pressed; Button has no pill-toggle variant
          <button
            key={chipKey(chip)}
            type="button"
            aria-pressed={selected}
            onClick={() => onToggle(chip)}
            className={cn(
              "inline-flex h-6 items-center gap-1 rounded-md border px-2 text-caption outline-none transition-colors motion-reduce:transition-none",
              "focus-visible:ring-2 focus-visible:ring-ring",
              selected
                ? "border-brand/45 bg-brand/15 text-foreground"
                : "border-border text-muted-foreground hover:bg-accent/50 hover:text-foreground",
            )}
          >
            {selected && <Check aria-hidden="true" className="size-3" />}
            {label}
          </button>
        );
      })}
      <p role="status" className="sr-only">
        {names.length > 0 ? t(($) => $.chips.active, { names: names.join(", ") }) : t(($) => $.chips.none)}
      </p>
    </div>
  );
}
