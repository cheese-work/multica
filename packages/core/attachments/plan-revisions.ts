import type { Attachment } from "../types/attachment";

export type PlanRevision = { attachment: Attachment; revision: number };
export type RevisionLine = {
  kind: "same" | "add" | "remove";
  text: string;
  oldNumber?: number;
  newNumber?: number;
};
export type RevisionSegment = {
  kind: "context" | "change";
  lines: RevisionLine[];
};

export function planRevisionNumber(filename: string): number | null {
  const match = /^plan-rev-([1-9]\d*)\.md$/.exec(filename);
  const revision = match ? Number(match[1]) : NaN;
  return Number.isSafeInteger(revision) ? revision : null;
}

export function earlierPlanRevisions(current: Attachment, attachments: Attachment[]): PlanRevision[] {
  const currentRevision = planRevisionNumber(current.filename);
  if (!current.issue_id || currentRevision === null) return [];
  return attachments.flatMap((attachment) => {
    const revision = planRevisionNumber(attachment.filename);
    return attachment.issue_id === current.issue_id && attachment.workspace_id === current.workspace_id && revision !== null && revision < currentRevision ? [{ attachment, revision }] : [];
  }).sort((left, right) => right.revision - left.revision || left.attachment.id.localeCompare(right.attachment.id));
}

export function defaultPlanBase(revisions: PlanRevision[]): string | null {
  const greatest = Math.max(...revisions.map((revision) => revision.revision));
  const candidates = revisions.filter((revision) => revision.revision === greatest);
  return candidates.length === 1 ? candidates[0]!.attachment.id : null;
}

export function diffRevisionLines(base: string, current: string): RevisionLine[] {
  const before = base === "" ? [] : base.replace(/\r\n/g, "\n").split("\n");
  const after = current === "" ? [] : current.replace(/\r\n/g, "\n").split("\n");
  if (before.length + after.length > 20_000) throw new Error("comparison_too_large");
  let prefix = 0;
  while (prefix < before.length && prefix < after.length && before[prefix] === after[prefix]) prefix += 1;
  let suffix = 0;
  while (suffix < before.length - prefix && suffix < after.length - prefix && before[before.length - suffix - 1] === after[after.length - suffix - 1]) suffix += 1;
  const oldEnd = before.length - suffix;
  const newEnd = after.length - suffix;
  const oldLength = oldEnd - prefix;
  const newLength = newEnd - prefix;
  if ((oldLength + 1) * (newLength + 1) > 4_000_000) throw new Error("comparison_too_large");
  const lengths = Array.from({ length: oldLength + 1 }, () => new Uint32Array(newLength + 1));
  for (let oldIndex = oldLength - 1; oldIndex >= 0; oldIndex -= 1) {
    for (let newIndex = newLength - 1; newIndex >= 0; newIndex -= 1) {
      lengths[oldIndex]![newIndex] = before[oldIndex + prefix] === after[newIndex + prefix] ? lengths[oldIndex + 1]![newIndex + 1]! + 1 : Math.max(lengths[oldIndex + 1]![newIndex]!, lengths[oldIndex]![newIndex + 1]!);
    }
  }
  const lines: RevisionLine[] = before.slice(0, prefix).map((text, index) => ({ kind: "same", text, oldNumber: index + 1, newNumber: index + 1 }));
  let oldIndex = prefix;
  let newIndex = prefix;
  while (oldIndex < oldEnd || newIndex < newEnd) {
    if (oldIndex < oldEnd && newIndex < newEnd && before[oldIndex] === after[newIndex]) lines.push({ kind: "same", text: before[oldIndex]!, oldNumber: ++oldIndex, newNumber: ++newIndex });
    else if (oldIndex < oldEnd && (newIndex === newEnd || lengths[oldIndex - prefix + 1]![newIndex - prefix]! >= lengths[oldIndex - prefix]![newIndex - prefix + 1]!)) lines.push({ kind: "remove", text: before[oldIndex]!, oldNumber: ++oldIndex });
    else lines.push({ kind: "add", text: after[newIndex]!, newNumber: ++newIndex });
  }
  for (let index = 0; index < suffix; index += 1) lines.push({ kind: "same", text: before[oldEnd + index]!, oldNumber: oldEnd + index + 1, newNumber: newEnd + index + 1 });
  return lines;
}

export function revisionSegments(lines: RevisionLine[]): RevisionSegment[] {
  const windows: { start: number; end: number }[] = [];
  lines.forEach((line, index) => {
    if (line.kind === "same") return;
    const start = Math.max(0, index - 3);
    const end = Math.min(lines.length, index + 4);
    const previous = windows.at(-1);
    if (previous && start <= previous.end) previous.end = end;
    else windows.push({ start, end });
  });
  const segments: RevisionSegment[] = [];
  let cursor = 0;
  for (const { start, end } of windows) {
    if (start > cursor) segments.push({ kind: "context", lines: lines.slice(cursor, start) });
    segments.push({ kind: "change", lines: lines.slice(start, end) });
    cursor = end;
  }
  if (cursor < lines.length) segments.push({ kind: "context", lines: lines.slice(cursor) });
  return segments;
}
