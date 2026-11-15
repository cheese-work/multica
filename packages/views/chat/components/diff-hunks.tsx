import { ArrowRight, Minus, Plus } from "@multica/ui/icons";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";

// New-side line count of a hunk: context and added lines, not removed ones.
const newSideLines = (lines: string[]) => lines.filter((line) => !line.startsWith("-")).length;

// Unchanged lines between two hunks, counted on the new side.
const hiddenBetween = (previous: DiffHunkLines | undefined, hunk: DiffHunkLines) =>
  previous ? hunk.new_start - (previous.new_start + newSideLines(previous.lines)) : 0;

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
        const hidden = hiddenBetween(previous, hunk);
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

type DiffRow = { key: string; old?: string; next?: string };

// Pair each run of removed lines with the run of added lines that follows it: the i-th removed line
// becomes the i-th added line's old value. Context lines are dropped: the card shows changes only.
function hunkRows(hunk: DiffHunkLines): DiffRow[] {
  const rows: DiffRow[] = [];
  let removed: string[] = [];
  let added: string[] = [];
  const flush = () => {
    const count = Math.max(removed.length, added.length);
    for (let i = 0; i < count; i++) rows.push({ key: `${hunk.new_start}:${rows.length}`, old: removed[i], next: added[i] });
    removed = [];
    added = [];
  };
  for (const line of hunk.lines) {
    if (line[0] === "-") {
      if (added.length > 0) flush();
      removed.push(line.slice(1));
    } else if (line[0] === "+") added.push(line.slice(1));
    else flush();
  }
  flush();
  return rows;
}

/**
 * The changed lines only, as rows (design direction B v9): old value struck through, new value in a
 * chip. Each row also says Changed, Added or Removed with an icon, so no state rides on colour alone.
 * Same hunks and the same hidden-lines row as `HunksRegion`; values are plain text.
 */
export function DiffRows({ hunks, label }: { hunks: DiffHunkLines[]; label: string }) {
  const { t } = useT("chat");
  return (
    <ul aria-label={label} className="max-h-72 space-y-1 overflow-auto break-words">
      {hunks.flatMap((hunk, index) => {
        const hidden = hiddenBetween(hunks[index - 1], hunk);
        return [
          ...(hidden > 0
            ? [<li key={`gap-${hunk.new_start}`} className="border-y border-dashed border-surface-border px-2.5 py-0.5 text-center text-caption italic text-muted-foreground">
                {t(($) => $.decision.artifacts.lines_hidden, { count: hidden })}
              </li>]
            : []),
          ...hunkRows(hunk).map(({ key, old, next }) => {
            const kind = old === undefined ? "added" : next === undefined ? "removed" : "changed";
            const Icon = kind === "added" ? Plus : kind === "removed" ? Minus : ArrowRight;
            return (
              <li key={`${hunk.old_start}:${key}`} className="flex flex-wrap items-baseline gap-x-2 gap-y-1 rounded-lg border border-surface-border px-2.5 py-1.5 text-label">
                <span className="inline-flex shrink-0 items-center gap-1 text-caption font-semibold text-muted-foreground">
                  <Icon className="size-3.5" aria-hidden />
                  {t(($) => $.decision.config_change[`row_${kind}`])}
                </span>
                {old !== undefined && <del className="text-muted-foreground line-through">{old}</del>}
                {next !== undefined && <ins className="rounded-full border border-surface-border bg-surface px-2.5 py-0.5 font-semibold text-foreground no-underline">{next}</ins>}
              </li>
            );
          }),
        ];
      })}
    </ul>
  );
}
