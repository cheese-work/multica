import { useCommentCollapseStore } from "./comment-collapse-store";
import { useResolvedExpandStore } from "./resolved-expand-store";
import { useIssueDisclosureStore } from "./issue-disclosure-store";

/**
 * Fold-all / unfold-all coordinate three independent Zustand stores (manual
 * collapse, resolved-bar expansion, unresolved-thread length preference —
 * 01-DESIGN "Fold all comments" / "Unfold all comments"). Each store's own
 * `setState` notifies its subscribers synchronously and independently, so a
 * subscriber that reads across stores (issue-detail.tsx passes
 * `expandedResolvedIds`/`threadLengthExpanded` as props derived from two of
 * these stores into CommentCard, which separately subscribes to the third)
 * can in principle observe an intermediate state where only some of the three
 * have applied.
 *
 * React 18+ automatic batching already coalesces three synchronous `set()`
 * calls with no `await`/microtask between them into a single re-render, so
 * this specific sequence has not been observed to tear any React consumer in
 * this codebase today. That protection is a property of *how* the calls are
 * invoked (synchronously, back-to-back, in the same tick), not something the
 * stores themselves guarantee — a future edit that awaits between two of the
 * three calls, or a subscriber outside React (a plain `store.subscribe`,
 * unaffected by React's batching), would silently reintroduce the tear this
 * module exists to prevent.
 *
 * Centralizing the three-call sequence here — instead of duplicating it in
 * both command handlers — makes "these three always change together,
 * synchronously, in one tick" a single, obviously-inspectable invariant
 * rather than an implicit consequence of two call sites happening to be
 * written the same way.
 */
export function foldAllCommentThreads(issueId: string, rootIds: readonly string[]): void {
  useCommentCollapseStore.getState().collapseAll(issueId, rootIds);
  useResolvedExpandStore.getState().collapseAll(issueId);
  useIssueDisclosureStore.getState().collapseAllThreads(issueId);
}

export function unfoldAllCommentThreads(
  issueId: string,
  rootIds: readonly string[],
  resolvedRootIds: readonly string[],
): void {
  useCommentCollapseStore.getState().expandAll(issueId);
  useResolvedExpandStore.getState().expandAll(issueId, resolvedRootIds);
  useIssueDisclosureStore.getState().expandAllThreads(issueId, rootIds);
}
