"use client";

import type { TaskInitContext, TaskInitContextPart } from "@multica/core/types";
import { useT } from "../../i18n";

const number = new Intl.NumberFormat();

// Wider label column than RunDetailRow: part names ("instructions") are longer.
function PartRow({ part }: { part: TaskInitContextPart }) {
  const { t } = useT("agents");
  const tokens = number.format(part.tokens);
  const overBudget = part.budget > 0 && part.tokens > part.budget;
  return (
    <div className="grid grid-cols-[6.5rem_minmax(0,1fr)] items-start gap-3 py-0.5">
      <span className="text-muted-foreground">{part.part}</span>
      <span className="min-w-0 select-text break-words text-foreground">
        <span>
          {part.budget > 0
            ? t(($) => $.transcript.details_init_context_part_budget, { tokens, budget: number.format(part.budget) })
            : t(($) => $.transcript.details_init_context_part, { tokens })}
        </span>
        {overBudget && (
          <span className="ml-2 text-destructive">{t(($) => $.transcript.details_init_context_over_budget)}</span>
        )}
        {part.notes?.map((note) => (
          <span key={note} className="block text-muted-foreground">
            {note}
          </span>
        ))}
      </span>
    </div>
  );
}

/** `result` is untyped JSON; older daemons never wrote `init_context`. */
export function initContextOf(result: unknown): TaskInitContext | undefined {
  const report = (result as { init_context?: TaskInitContext } | null)?.init_context;
  return report ?? undefined;
}

/** Where the run's first-prompt tokens went, per part. Renders nothing without a report. */
export function RunInitContextDetails({ report }: { report: TaskInitContext | undefined }) {
  const { t } = useT("agents");
  if (!Array.isArray(report?.parts) || report.parts.length === 0) return null;
  const total = number.format(report.parts.reduce((sum, p) => sum + p.tokens, 0));
  return (
    <>
      <div className="my-2 h-px bg-border" />
      <div className="grid grid-cols-[6.5rem_minmax(0,1fr)] items-start gap-3 py-0.5">
        <span className="text-muted-foreground">{t(($) => $.transcript.details_init_context)}</span>
        <span className="min-w-0 select-text text-foreground">
          {t(($) => $.transcript.details_init_context_total, { tokens: total })}
        </span>
      </div>
      {report.parts.map((part) => (
        <PartRow key={part.part} part={part} />
      ))}
    </>
  );
}
