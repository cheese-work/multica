import type { LintCounts } from "@multica/core/issue-workflows";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../i18n";
import { LintLevelIcon } from "./lint-level";

/**
 * Error, warning and note counts as icon + number. Used by the Checks button
 * and the step cards, so both read the same lint result. Renders nothing when
 * every count is zero.
 */
export function LintMarks({ counts, className }: { counts: LintCounts; className?: string }) {
  const { t } = useT("issues");
  const marks = [
    { level: "error" as const, count: counts.errors, label: t(($) => $.workflows.lint.level_error, { count: counts.errors }) },
    { level: "warning" as const, count: counts.warnings, label: t(($) => $.workflows.lint.level_warning, { count: counts.warnings }) },
    { level: "info" as const, count: counts.infos, label: t(($) => $.workflows.lint.level_note, { count: counts.infos }) },
  ].filter((m) => m.count > 0);
  if (marks.length === 0) return null;
  return (
    <span className={cn("inline-flex items-center gap-2 text-caption", className)}>
      {marks.map((m) => (
        <span key={m.level} className="inline-flex items-center gap-1">
          <LintLevelIcon level={m.level} />
          <b aria-label={m.label} className="font-mono font-medium tabular-nums">
            {m.count}
          </b>
        </span>
      ))}
    </span>
  );
}
