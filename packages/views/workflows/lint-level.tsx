import { CircleAlert, Info, TriangleAlert } from "@multica/ui/icons";
import { cn } from "@multica/ui/lib/utils";
import type { LintLevel } from "@multica/core/issue-workflows";

/** Level is an icon plus a word or number, never colour alone. */
export function LintLevelIcon({ level, className }: { level: LintLevel; className?: string }) {
  const Icon = level === "error" ? CircleAlert : level === "warning" ? TriangleAlert : Info;
  return (
    <Icon
      aria-hidden
      className={cn(
        "size-3.5 shrink-0",
        level === "error" ? "text-destructive" : level === "warning" ? "text-warning" : "text-info",
        className,
      )}
    />
  );
}
