import type { Attachment } from "../types";

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

/**
 * Issue-level uploads that belong to a revision chain. An upload bound to the
 * issue but to no comment has no other surface; a lone first revision stays
 * hidden as before, a chain shows (the list collapses it to its latest).
 */
export function chainedIssueAttachments(all: Attachment[] | undefined): Attachment[] {
  const issueLevel = (all ?? []).filter((a) => !a.comment_id);
  const superseded = new Set(issueLevel.map((a) => a.revision_of).filter(Boolean));
  return issueLevel.filter((a) => (a.revision ?? 1) >= 2 || superseded.has(a.id));
}
