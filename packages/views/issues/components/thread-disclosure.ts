import type { TimelineEntry } from "@multica/core/types";
import type { CommentRun } from "./comment-runs";

/** A published reply's own chronological slot; anchor association is metadata only. */
export interface ThreadCommentSlot {
  kind: "comment";
  comment: TimelineEntry;
  /** Runs whose reply landed on this comment, deduplicated, ordered by task created_at/id. */
  runs: CommentRun[];
}

/** A run that has not published a reply yet, placed after the visible anchor it covers. */
export interface ThreadRunSlot {
  kind: "run";
  run: CommentRun;
}

export type ThreadDisplaySlot = ThreadCommentSlot | ThreadRunSlot;

export interface ThreadDisplayProjection {
  /** Ordered slots for the visible portion of the thread (root excluded). */
  slots: ThreadDisplaySlot[];
  /** Unique logical replies omitted from `slots` in compact mode. */
  hiddenCount: number;
}

function runTaskOrder(a: CommentRun, b: CommentRun): number {
  return a.task.created_at.localeCompare(b.task.created_at) || a.task.id.localeCompare(b.task.id);
}

/**
 * Canonical, pure thread projection. Replaces recursive anchor placement
 * (`renderAnchoredRuns`/`slottedReplyIds`) with a single forward pass: every
 * loaded reply owns exactly one chronological slot regardless of which run
 * anchors to it, and anchor chains never recurse into another comment's body.
 *
 * `replies` must already be in canonical chronological order (ascending
 * created_at, then id) — the order `collectThreadReplies` returns. `runs` are
 * the root's `CommentRun[]` from `buildCommentRunView`, unfiltered.
 *
 * `expanded` selects "all replies" vs D-04's "latest three logical replies";
 * the root itself is never part of the returned slots or hiddenCount.
 */
export function projectThreadDisplay(
  replies: readonly TimelineEntry[],
  runs: readonly CommentRun[],
  expanded: boolean,
): ThreadDisplayProjection {
  const repliesById = new Map(replies.map((reply) => [reply.id, reply]));

  // Runs that published a reply attach to that reply's own slot, never to the
  // anchor's slot — this is what retires recursive anchor placement. Multiple
  // runs on the same reply dedupe into one slot, ordered by task identity.
  const runsByReplyId = new Map<string, CommentRun[]>();
  // Runs without a published reply attach after their anchor, but only when
  // the anchor is itself a loaded, visible reply — an anchor that is hidden,
  // missing, or not a reply in this thread never surfaces its run-only slot
  // here (a hidden anchor's active run is expected to force the thread open
  // upstream of this function, not be synthesized by it).
  const runOnlySlotsByAnchor = new Map<string, CommentRun[]>();

  for (const run of runs) {
    if (run.hasReply && run.commentId && repliesById.has(run.commentId)) {
      const list = runsByReplyId.get(run.commentId) ?? [];
      list.push(run);
      runsByReplyId.set(run.commentId, list);
      continue;
    }
    if (!run.hasReply && run.anchorCommentId && repliesById.has(run.anchorCommentId)) {
      const list = runOnlySlotsByAnchor.get(run.anchorCommentId) ?? [];
      list.push(run);
      runOnlySlotsByAnchor.set(run.anchorCommentId, list);
    }
  }
  for (const list of runsByReplyId.values()) list.sort(runTaskOrder);
  for (const list of runOnlySlotsByAnchor.values()) list.sort(runTaskOrder);

  // Deduplicate logical comment IDs before selecting the visible window —
  // the same reply must never appear twice regardless of how many runs
  // reference it.
  const uniqueReplies: TimelineEntry[] = [];
  const seen = new Set<string>();
  for (const reply of replies) {
    if (seen.has(reply.id)) continue;
    seen.add(reply.id);
    uniqueReplies.push(reply);
  }

  const visibleReplies = expanded || uniqueReplies.length <= 3
    ? uniqueReplies
    : uniqueReplies.slice(-3);
  const hiddenCount = uniqueReplies.length - visibleReplies.length;

  // A hidden anchor's run-only slot is dropped with its region (never emitted
  // below) — surfacing it would leak the hidden reply's presence outside its
  // logical slot.
  const slots: ThreadDisplaySlot[] = [];
  for (const reply of visibleReplies) {
    slots.push({ kind: "comment", comment: reply, runs: runsByReplyId.get(reply.id) ?? [] });
    for (const run of runOnlySlotsByAnchor.get(reply.id) ?? []) {
      slots.push({ kind: "run", run });
    }
  }

  return { slots, hiddenCount };
}
