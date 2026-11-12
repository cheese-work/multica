export interface DiffHunk {
  old_start: number;
  new_start: number;
  lines: string[];
}

const HUNK_HEADER = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/;

/**
 * Hunks of a unified diff, each line keeping its " ", "-" or "+" prefix.
 * Text before the first `@@` (the `---`/`+++` names) is dropped. A hunk line
 * is never matched against `---`, so "--- rule" removed from a file stays a line.
 */
export function parseUnifiedDiff(text: string): DiffHunk[] {
  const hunks: DiffHunk[] = [];
  for (const line of text.split("\n")) {
    const header = HUNK_HEADER.exec(line);
    if (header) {
      hunks.push({ old_start: Number(header[1]), new_start: Number(header[2]), lines: [] });
    } else if (line && hunks.length > 0) {
      hunks[hunks.length - 1]!.lines.push(line);
    }
  }
  return hunks;
}
