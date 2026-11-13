/**
 * Screen-scoped state for "compose a reply to comment X" — read by the
 * inline comment composer, written by the comment long-press action sheet
 * ("Reply").
 *
 * Why a global Zustand store and not props:
 *   - The trigger (long-press action sheet) and the consumer (composer at
 *     the bottom of the screen) live in different component trees. Threading
 *     a callback through TimelineList → CommentCard → useCommentLongPress
 *     would mean three layers of prop drilling for one boolean signal.
 *   - Only one reply can be in-flight at a time per issue detail screen,
 *     so a singleton store is the right shape.
 *
 * Lifecycle: this store is the live copy. `InlineCommentComposer` loads it
 * from the issue's composer draft (`composer-drafts-store`), saves every
 * change back, and clears it on unmount, so a stale target can't leak across
 * issues and an unsent reply survives leaving the issue.
 */
import { create } from "zustand";

export interface ReplyTarget {
  commentId: string;
  /** Display name for the "Replying to X" chip above the input. Resolved
   *  at trigger time via `useActorLookup().getName(actor_type, actor_id)`
   *  so the chip doesn't have to do its own actor lookup. */
  actorName: string;
  /** Raw markdown of the parent comment. The chip renders it via
   *  `stripMarkdown()` + `numberOfLines={2}` so the user sees what they're
   *  replying to without leaving the keyboard. May be empty string for
   *  attachment-only comments. */
  preview: string;
  /** Set when the target was loaded from a saved draft rather than just
   *  picked: the composer opens but does not take focus. */
  restored?: boolean;
}

interface State {
  target: ReplyTarget | null;
  setTarget: (target: ReplyTarget) => void;
  clear: () => void;
}

export const useReplyTargetStore = create<State>((set) => ({
  target: null,
  setTarget: (target) => set({ target }),
  clear: () => set({ target: null }),
}));
