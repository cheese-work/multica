import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";

// New-side line count of a hunk: context and added lines, not removed ones.
const newSideLines = (lines: string[]) => lines.filter((line) => !line.startsWith("-")).length;

export interface DiffHunkLines {
  old_start: number;
  new_start: number;
  lines: string[];
}

// The focusable, labelled scroll region of changed hunks, shared by the artifact viewer and the
// config-change viewer. Each line carries a one-character prefix: "+", "-" or a space.
export function HunksRegion({ hunks, label }: { hunks: DiffHunkLines[]; label: string }) {
  const { t } = useT("chat");
  return (
    <div
      role="region"
      tabIndex={0}
      aria-label={label}
      className="min-h-0 flex-1 overflow-auto rounded-lg border border-surface-border bg-surface font-mono text-caption leading-6 focus-visible:outline-2 focus-visible:outline-ring"
    >
      {hunks.map((hunk, index) => {
        const previous = hunks[index - 1];
        const hidden = previous ? hunk.new_start - (previous.new_start + newSideLines(previous.lines)) : 0;
        let number = hunk.new_start;
        return (
          <div key={`${hunk.old_start}:${hunk.new_start}`}>
            {hidden > 0 && (
              <div className="border-y border-dashed border-surface-border px-2.5 py-0.5 text-center italic text-muted-foreground">
                {t(($) => $.decision.artifacts.lines_hidden, { count: hidden })}
              </div>
            )}
            {hunk.lines.map((line, lineIndex) => {
              const kind = line[0];
              const shown = kind === "-" ? "" : number++;
              return (
                <div
                  key={lineIndex}
                  className={cn(
                    "grid grid-cols-[2rem_1.1rem_1fr] whitespace-pre-wrap break-words",
                    kind === "+" && "bg-success/10",
                    kind === "-" && "bg-destructive/10",
                  )}
                >
                  <i className="pr-1.5 text-right not-italic text-muted-foreground">{shown}</i>
                  <s className={cn("text-center font-bold no-underline", kind === "+" && "text-success", kind === "-" && "text-destructive")}>
                    {kind === "+" ? "+" : kind === "-" ? "−" : ""}
                  </s>
                  <code>{line.slice(1)}</code>
                </div>
              );
            })}
          </div>
        );
      })}
    </div>
  );
}
