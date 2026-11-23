import { useId, useState } from "react";
import { countDiffLines } from "@multica/core/issues/decision-artifacts";
import { workflowStepTarget } from "@multica/core/issues/decision-action-preview";
import type { DecisionConfigChange } from "@multica/core/types";
import { useT } from "../../i18n";
import { DiffRows } from "./diff-hunks";

// The value that will be written, as the server stored it: text as is, lines one per row, JSON indented.
function wholeText({ diff_kind, after }: DecisionConfigChange): string {
  if (typeof after === "string") return after;
  if (diff_kind === "lines" && Array.isArray(after)) return after.map(String).join("\n");
  return JSON.stringify(after, null, 2) ?? "";
}

type SectionItem = { name: string; start_line?: number; end_line?: number };

// The server's section scope (meta.section), or undefined when it is absent or malformed.
function sectionScope(meta: DecisionConfigChange["meta"]): { kind: string; items: SectionItem[] } | undefined {
  const section = meta?.section;
  if (typeof section !== "object" || section === null) return undefined;
  const { kind, items } = section as { kind?: unknown; items?: unknown };
  if (typeof kind !== "string" || !Array.isArray(items) || items.length === 0) return undefined;
  const wellFormed = items.every((item) => typeof item === "object" && item !== null && typeof (item as SectionItem).name === "string");
  return wellFormed ? { kind, items: items as SectionItem[] } : undefined;
}

// Only the section the server says changed, as written. Anything the scope does not fit (a range past
// the text, a step or key the value lacks, an unknown kind) opens the whole value: showing more is safe.
function sectionText(change: DecisionConfigChange): string | undefined {
  const scope = sectionScope(change.meta);
  if (!scope) return undefined;
  const { after } = change;
  const names = new Set(scope.items.map((item) => item.name));
  switch (scope.kind) {
    case "heading": {
      if (typeof after !== "string") return undefined;
      const lines = after.replace(/\r\n/g, "\n").replace(/\n$/, "").split("\n");
      const chunks = scope.items.map(({ start_line: start, end_line: end }) =>
        Number.isInteger(start) && Number.isInteger(end) && start! >= 1 && start! <= end! && end! <= lines.length
          ? lines.slice(start! - 1, end!).join("\n")
          : undefined);
      return chunks.every((chunk) => chunk !== undefined) ? chunks.join("\n\n") : undefined;
    }
    case "step": {
      if (!Array.isArray(after)) return undefined;
      const steps = after.filter((step) => typeof step === "object" && step !== null && names.has((step as { status_key?: string }).status_key ?? ""));
      return steps.length === names.size ? JSON.stringify(steps, null, 2) : undefined;
    }
    case "key": {
      if (typeof after !== "object" || after === null || Array.isArray(after)) return undefined;
      const picked = Object.fromEntries(Object.entries(after).filter(([key]) => names.has(key)));
      return Object.keys(picked).length === names.size ? JSON.stringify(picked, null, 2) : undefined;
    }
    default:
      return undefined;
  }
}

function resultText(change: DecisionConfigChange): string {
  return sectionText(change) ?? wholeText(change);
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
 * The "you are approving" viewer for a config change (design direction B v9). It shows what the server
 * stored as the preview: the field, the target, the changed rows only (old struck through, new in a
 * chip; `DiffRows`, built on the same hunks as the artifact viewer) and a Show full button that opens
 * the value that will be written, only the changed section when the server scoped one (CHE-5703),
 * with no − or + marks. Everything is rendered as text.
 */
export function ConfigChangeViewer({ change }: { change: DecisionConfigChange }) {
  const { t } = useT("chat");
  const fullId = useId();
  const [showFull, setShowFull] = useState(false);

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
  return (
    <div data-testid="config-change-viewer" className="space-y-2 rounded-lg border border-surface-border p-2.5 text-label">
      <div className="space-y-0.5">
        <p className="font-semibold">{field} <span className="text-caption font-normal text-muted-foreground">+{added} −{removed}</span></p>
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
      {change.diff.length === 0 ? (
        <p className="text-caption text-muted-foreground">
          {change.meta?.whitespace_only === true
            ? t(($) => $.decision.config_change.whitespace_only)
            : t(($) => $.decision.config_change.no_changes)}
        </p>
      ) : (
        <DiffRows hunks={change.diff} label={t(($) => $.decision.config_change.changes_region, { field })} />
      )}
      {/* ui-pattern-ref: allow inline disclosure toggle (aria-expanded) matching the card Details link; Button has no text-disclosure variant */}
      <button
        type="button"
        aria-expanded={showFull}
        aria-controls={fullId}
        onClick={() => setShowFull((open) => !open)}
        className="inline-flex min-h-9 items-center rounded-lg border border-surface-border px-3 text-label font-semibold hover:bg-muted focus-visible:outline-2 focus-visible:outline-ring"
      >
        {showFull ? t(($) => $.decision.config_change.hide_full) : t(($) => $.decision.config_change.show_full)}
      </button>
      <div id={fullId} hidden={!showFull}>
        {showFull && (
          <div
            role="region"
            tabIndex={0}
            aria-label={t(($) => $.decision.config_change.result_region)}
            className="max-h-72 overflow-auto whitespace-pre-wrap break-words rounded-lg border border-surface-border bg-surface px-3 py-2.5 font-mono text-caption focus-visible:outline-2 focus-visible:outline-ring"
          >
            {resultText(change)}
          </div>
        )}
      </div>
    </div>
  );
}
