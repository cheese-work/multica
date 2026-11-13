import { useId, useRef, useState, type KeyboardEvent } from "react";
import { countDiffLines } from "@multica/core/issues/decision-artifacts";
import { workflowStepTarget } from "@multica/core/issues/decision-action-preview";
import type { DecisionConfigChange } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { HunksRegion } from "./diff-hunks";

type TabKey = "changes" | "result";
const TABS: readonly TabKey[] = ["changes", "result"];

// The value that will be written, as the server stored it: text as is, lines one per row, JSON indented.
function resultText({ diff_kind, after }: DecisionConfigChange): string {
  if (typeof after === "string") return after;
  if (diff_kind === "lines" && Array.isArray(after)) return after.map(String).join("\n");
  return JSON.stringify(after, null, 2) ?? "";
}

// The server's counts of a text change, in the unit it counted in. Shown as sent, never recounted;
// anything but non-negative whole numbers in a known unit is not shown.
function serverCounts(meta: DecisionConfigChange["meta"]): { before: number; after: number; cap?: number } | undefined {
  const { unit, chars_before: before, chars_after: after, cap } = meta ?? {};
  const count = (value: unknown): value is number => typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
  if (unit !== "code_points" || !count(before) || !count(after)) return undefined;
  return { before, after, ...(count(cap) ? { cap } : {}) };
}

/**
 * The "you are approving" viewer for a config change (design `you-are-approving-card`, direction B).
 * It shows what the server stored as the preview, never anything the client derived: the field, the
 * target, a Changes tab (the stored diff) and a Result tab (the value that will be written). Collapsed
 * until a tab opens; selecting the open tab closes it. Everything is rendered as text.
 */
export function ConfigChangeViewer({ change }: { change: DecisionConfigChange }) {
  const { t } = useT("chat");
  const id = useId();
  const [open, setOpen] = useState<TabKey | null>(null);
  const [focused, setFocused] = useState<TabKey>("changes");
  const refs = useRef<Partial<Record<TabKey, HTMLButtonElement | null>>>({});

  const field = (() => {
    switch (change.field) {
      case "agent.instructions": return t(($) => $.decision.config_change.field_agent_instructions);
      case "squad.instructions": return t(($) => $.decision.config_change.field_squad_instructions);
      case "workspace.context": return t(($) => $.decision.config_change.field_workspace_context);
      case "workflow.steps": return t(($) => $.decision.config_change.field_workflow_steps);
      case "workflow.instructions": return t(($) => $.decision.config_change.field_workflow_instructions);
      case "workflow.budget": return t(($) => $.decision.config_change.field_workflow_budget);
      case "workspace.routing_agent": return t(($) => $.decision.config_change.field_workspace_routing_agent);
      case "workspace.delegated_writes": return t(($) => $.decision.config_change.field_workspace_delegated_writes);
      case "agent.skills": return t(($) => $.decision.config_change.field_agent_skills);
      case "workspace.settings": return t(($) => $.decision.config_change.field_workspace_settings);
      default: return change.field;
    }
  })();
  const stepTarget = change.field === "workflow.instructions" ? workflowStepTarget(change) : undefined;
  const { added, removed } = countDiffLines(change.diff);
  const counts = serverCounts(change.meta);
  const number = new Intl.NumberFormat();
  const tabId = (key: TabKey) => `${id}-tab-${key}`;

  const onKeyDown = (event: KeyboardEvent, key: TabKey) => {
    const index = TABS.indexOf(key);
    const target =
      event.key === "ArrowRight" || event.key === "ArrowDown" ? TABS[(index + 1) % TABS.length]
      : event.key === "ArrowLeft" || event.key === "ArrowUp" ? TABS[(index - 1 + TABS.length) % TABS.length]
      : event.key === "Home" ? TABS[0]
      : event.key === "End" ? TABS[TABS.length - 1]
      : undefined;
    if (!target) return;
    event.preventDefault();
    setFocused(target);
    refs.current[target]?.focus();
  };

  return (
    <div data-testid="config-change-viewer" className="space-y-2 rounded-lg border border-surface-border p-2.5 text-label">
      <div className="space-y-0.5">
        <p className="font-semibold">{field}</p>
        <p className="break-all text-caption text-muted-foreground">
          {t(($) => $.decision.config_change.target)}: <code className="font-mono">{change.target_id}</code>
        </p>
        {stepTarget && (
          <>
            <p data-testid="config-change-workflow" className="break-all text-caption text-muted-foreground">
              {t(($) => $.decision.config_change.workflow)}: <span className="font-semibold text-foreground">{stepTarget.workflowName}</span>
            </p>
            <p data-testid="config-change-status" className="break-all text-caption text-muted-foreground">
              {t(($) => $.decision.config_change.step_status)}: <code className="font-mono">{stepTarget.statusKey}</code>
            </p>
          </>
        )}
        {counts && (
          <p data-testid="config-change-counts" className="text-caption text-muted-foreground">
            {counts.cap === undefined
              ? t(($) => $.decision.config_change.counts, { before: number.format(counts.before), after: number.format(counts.after) })
              : t(($) => $.decision.config_change.counts_cap, { before: number.format(counts.before), after: number.format(counts.after), cap: number.format(counts.cap) })}
          </p>
        )}
        {change.keys.length > 0 && (
          <p data-testid="config-change-keys" className="break-all text-caption text-muted-foreground">
            {t(($) => $.decision.config_change.keys)}: <code className="font-mono">{change.keys.join(", ")}</code>
          </p>
        )}
      </div>
      <div role="tablist" aria-label={t(($) => $.decision.config_change.viewer_label)} className="flex gap-0.5">
        {TABS.map((key) => (
          <button
            key={key}
            ref={(node) => { refs.current[key] = node; }}
            type="button"
            role="tab"
            id={tabId(key)}
            data-testid={`config-change-tab-${key}`}
            aria-selected={open === key}
            aria-controls={`${id}-panel`}
            tabIndex={focused === key ? 0 : -1}
            onFocus={() => setFocused(key)}
            onKeyDown={(event) => onKeyDown(event, key)}
            onClick={() => setOpen(open === key ? null : key)}
            className={cn(
              "inline-flex min-h-9 items-center gap-1.5 whitespace-nowrap rounded-lg px-3 text-label font-semibold focus-visible:outline-2 focus-visible:outline-ring",
              open === key ? "bg-primary/10 text-foreground" : "text-muted-foreground hover:bg-muted",
            )}
          >
            {key === "changes" ? t(($) => $.decision.config_change.tab_changes) : t(($) => $.decision.config_change.tab_result)}
            {key === "changes" && <span className="text-caption font-normal text-muted-foreground">+{added} −{removed}</span>}
          </button>
        ))}
      </div>
      <div
        role="tabpanel"
        id={`${id}-panel`}
        data-testid="config-change-panel"
        aria-labelledby={tabId(open ?? focused)}
        hidden={open === null}
        className="flex max-h-72 min-h-0 flex-col"
      >
        {open === "changes" && (
          change.diff.length === 0 ? (
            <p className="text-caption text-muted-foreground">
              {change.meta?.whitespace_only === true
                ? t(($) => $.decision.config_change.whitespace_only)
                : t(($) => $.decision.config_change.no_changes)}
            </p>
          ) : (
            <HunksRegion hunks={change.diff} label={t(($) => $.decision.config_change.changes_region, { field })} />
          )
        )}
        {open === "result" && (
          <div
            role="region"
            tabIndex={0}
            aria-label={t(($) => $.decision.config_change.result_region)}
            className="min-h-0 flex-1 overflow-auto whitespace-pre-wrap break-words rounded-lg border border-surface-border bg-surface px-3 py-2.5 font-mono text-caption focus-visible:outline-2 focus-visible:outline-ring"
          >
            {resultText(change)}
          </div>
        )}
      </div>
    </div>
  );
}
