"use client";

import { memo } from "react";
import { useQuery } from "@tanstack/react-query";
import { Bell, PauseCircle, TriangleAlert } from "@multica/ui/icons";
import { issueSystemWakeupsOptions, issueWakeupsOptions } from "@multica/core/issues";
import type { IssueWakeup, SystemWakeup } from "@multica/core/types";
import { useWorkspaceId } from "@multica/core/hooks";
import { useCurrentWorkspace } from "@multica/core/paths";
import { deriveWakeupsPause } from "@multica/core/workspace/wakeups-pause";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { useWakeupText } from "./wakeup-presentation";

/**
 * Picks what the header says an issue is waiting for: the first rule waiting
 * on an event or condition, else the soonest scheduled one, else the
 * sub-issue system rule. A rule the platform paused is shown only when
 * nothing else is waiting, because it needs someone to look at it.
 */
export function primaryWakeup(rules: readonly IssueWakeup[], system: readonly SystemWakeup[]) {
  const enabled = rules.filter((w) => w.enabled);
  const waiting = enabled.filter((w) => w.kind === "event");
  const scheduled = enabled
    .filter((w) => w.kind !== "event")
    .sort((a, b) => Date.parse(a.next_fire_at ?? "") - Date.parse(b.next_fire_at ?? ""));
  const systemRule = system.find((r) => r.enabled && !r.blocked && r.target);
  const count = enabled.length + (systemRule ? 1 : 0);
  const rule = waiting[0] ?? scheduled[0];
  if (rule) return { kind: "rule" as const, rule, count };
  if (systemRule) return { kind: "system" as const, rule: systemRule, count };
  const paused = rules.find((w) => !w.enabled && w.paused_reason);
  if (paused) return { kind: "paused" as const, rule: paused, count: 1 };
  return null;
}

/**
 * One line in the issue header saying what the issue is waiting for, e.g.
 * "Emacs is waiting for Jiayuan to reply +2". Opens the Wakeups section.
 */
export const IssueWakeupHeaderChip = memo(function IssueWakeupHeaderChip({
  issueId,
  onOpen,
}: {
  issueId: string;
  onOpen: () => void;
}) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const text = useWakeupText();
  const { data: rules = [] } = useQuery(issueWakeupsOptions(wsId, issueId));
  const { data: system = [] } = useQuery(issueSystemWakeupsOptions(wsId, issueId));
  const primary = primaryWakeup(rules, system);
  const workspacePaused = deriveWakeupsPause(useCurrentWorkspace()).paused;
  if (!primary) return null;
  // A paused workspace fires nothing, so the chip must not promise a due time.
  const label = String(
    workspacePaused
      ? t(($) => $.wakeups.wait.workspace_paused)
      : primary.kind === "rule"
      ? text.headline(primary.rule)
      : primary.kind === "system"
        ? t(($) => $.wakeups.wait.header, {
            agent: primary.rule.target?.name ?? "",
            what:
              primary.rule.staged && primary.rule.stage !== null
                ? t(($) => $.wakeups.wait.children_stage, { stage: primary.rule.stage })
                : t(($) => $.wakeups.wait.children_all),
          })
        : t(($) => $.wakeups.wait.paused),
  );
  const Icon = workspacePaused ? PauseCircle : primary.kind === "paused" ? TriangleAlert : Bell;
  return (
    <button
      type="button"
      data-testid="wakeups-header-chip"
      onClick={onOpen}
      title={label}
      aria-label={`${label}${primary.count > 1 ? ` +${primary.count - 1}` : ""} · ${t(($) => $.wakeups.wait.open)}`}
      className={cn(
        "inline-flex h-7 max-w-72 min-w-0 items-center gap-1.5 rounded-full border border-border px-2.5 text-caption hover:bg-accent focus-visible:outline-2 focus-visible:outline-ring",
        workspacePaused || primary.kind === "paused" ? "text-warning" : "text-foreground",
      )}
    >
      <Icon className="size-3.5 shrink-0" aria-hidden="true" />
      <span className="min-w-0 truncate max-sm:hidden">{label}</span>
      {primary.count > 1 && (
        <span className="shrink-0 tabular-nums text-muted-foreground">+{primary.count - 1}</span>
      )}
    </button>
  );
});
