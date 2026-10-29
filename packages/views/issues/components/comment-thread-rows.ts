import type { TimelineEntry } from "@multica/core/types";
import { isDeletedComment } from "@multica/core/issues/comment-deletion";
import { orderThreadWithRuns, type CommentRun } from "./comment-runs";
import { deriveThreadResolution } from "./thread-utils";
import { projectThreadDisplay } from "./thread-disclosure";

export type CommentThreadRow = {
  kind: "root" | "controls" | "reply" | "footer";
  id: string;
  key?: string;
  last?: boolean;
};

export function projectCommentThread(
  entry: TimelineEntry,
  replies: TimelineEntry[],
  runs: CommentRun[],
  lengthExpanded: boolean,
  threadExpanded: boolean,
  open = true,
) {
  const visibleReplies = replies.filter(reply => !isDeletedComment(reply));
  const resolution = deriveThreadResolution(entry, replies);
  const replyResolutionId = resolution.kind === "reply" ? resolution.resolutionId : null;
  const replyFolded = replyResolutionId != null && !threadExpanded;
  const foldedReplies = replyResolutionId
    ? visibleReplies.filter(reply => reply.id !== replyResolutionId) : visibleReplies;
  const resolutionReply = replies.find(reply => reply.id === replyResolutionId) ?? null;
  const nestedRuns = runs.filter(run => run.anchorCommentId !== entry.id);
  const rootRunReplyIds = new Set(runs.filter(run => run.anchorCommentId === entry.id && run.hasReply && run.commentId)
    .map(run => run.commentId!));
  const hiddenFromThreadWindow = new Set(rootRunReplyIds);
  for (const reply of replies) if (isDeletedComment(reply)) hiddenFromThreadWindow.add(reply.id);
  const threadProjection = projectThreadDisplay(replies, nestedRuns, lengthExpanded, hiddenFromThreadWindow);
  const canShowLess = replies.length - hiddenFromThreadWindow.size > 3;
  const selectedReplyIds = new Set(threadProjection.slots.flatMap(slot => slot.kind === "comment" ? [slot.comment.id] : []));
  const threadRows = orderThreadWithRuns(entry,
    replies.filter(reply => selectedReplyIds.has(reply.id) || rootRunReplyIds.has(reply.id)),
    [...runs.filter(run => run.anchorCommentId === entry.id),
      ...threadProjection.slots.flatMap(slot => slot.kind === "run" ? [slot.run] : [])]);
  const rows: CommentThreadRow[] = [{ kind: "root", id: entry.id }];
  if (open) {
    if (replyFolded) {
      rows.push(...runs.filter(run => run.anchorCommentId === entry.id && !run.hasReply)
        .map(run => ({ kind: "reply" as const, id: run.task.id, key: run.task.id })));
      if (foldedReplies.length) rows.push({ kind: "controls", id: `thread-controls-${entry.id}` });
      if (resolutionReply) rows.push({ kind: "reply", id: resolutionReply.id });
    } else {
      if (replyResolutionId || (!lengthExpanded && threadProjection.hiddenCount)) {
        rows.push({ kind: "controls", id: `thread-controls-${entry.id}` });
      }
      rows.push(...threadRows.map(row => "run" in row
        ? { kind: "reply" as const, id: row.reply?.id ?? row.run.task.id, key: row.run.task.id }
        : { kind: "reply" as const, id: row.id }));
    }
    if (!replyFolded && rows.length > 1) rows.push({ kind: "footer", id: `thread-footer-${entry.id}` });
  }
  rows[rows.length - 1]!.last = true;
  return { visibleReplies, replyResolutionId, replyFolded, foldedReplies, resolutionReply,
    threadProjection, canShowLess, threadRows, rows };
}
