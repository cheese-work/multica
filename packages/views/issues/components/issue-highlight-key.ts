/**
 * Memento entry recording that the comment-highlight deep link for this
 * issue already landed on the current route (value: the consumed comment
 * id). Lets a remount that merely restores the view (tab switch back) skip
 * the jump-and-highlight, while a fresh selection — which clears the entry
 * (see InboxPage.handleSelect) — runs it again.
 */
export function issueHighlightMementoKey(issueId: string): string {
  return `highlight:${issueId}`;
}
