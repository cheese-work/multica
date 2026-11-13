"use client";

import { isStoppedByBudget } from "@multica/core/run-budget";
import { useT } from "../../i18n";

/** Names an issue the run budget stopped, next to its status, so it does not read as a plain blocked issue. */
export function RunBudgetChip({
  issue,
  className = "",
}: {
  issue: { status: string; run_budget_blocked?: boolean };
  className?: string;
}) {
  const { t } = useT("issues");
  if (!isStoppedByBudget(issue)) return null;
  return (
    <span
      data-testid="run-budget-chip"
      className={`inline-flex items-center rounded-full bg-destructive/10 px-1.5 py-0.5 text-micro font-medium text-destructive ${className}`}
    >
      {t(($) => $.run_budget.chip)}
    </span>
  );
}
