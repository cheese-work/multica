"use client";

import { useLayoutEffect, useRef, useState } from "react";

// ---------------------------------------------------------------------------
// Reveal-before-DOM-walk generation token (01-DESIGN.md "Find and target
// reveal lifecycle").
//
// In-page find and notification/deep-link targets both need to walk the DOM
// only AFTER the issue view has actually force-revealed every fold that
// participates in the current reveal request (description, resolved-bar,
// manually-collapsed thread, length-folded thread). Deriving `forceRevealAll`
// from `find.open` (or a target pin) happens in the SAME render as every
// other state read, but React does not paint/commit synchronously with that
// render — a MutationObserver callback or a query-driven effect can still run
// before the flattened, force-open DOM has actually landed.
//
// This hook is the single source of truth for "has the current reveal
// request's DOM actually committed yet": it publishes a monotonically
// increasing generation number from a layout effect, which by definition only
// runs after React has committed the corresponding DOM mutations. Consumers
// (use-in-page-find's collector, issue-detail's target-landing effect) treat
// "the token's (issueId, revealKey) matches what I am currently revealing"
// as the ONLY allowed signal that the DOM is safe to walk — never a fixed
// rAF delay or a timer.
// ---------------------------------------------------------------------------

export interface DisclosureRevealToken {
  /** Issue this generation was committed for. */
  readonly issueId: string;
  /**
   * Caller-supplied key identifying the reveal request this generation
   * commits (e.g. a find "session" id bumped on every find.open, or a target
   * request token). Opaque to this hook — it only has to be stable while the
   * same reveal request is in flight, and change when a new one starts.
   */
  readonly revealKey: string;
  /** Monotonically increasing across every commit for this issue. */
  readonly generation: number;
}

export interface UseIssueDisclosureRevealOptions {
  /** Issue whose disclosure state this reveal token tracks. */
  readonly issueId: string;
  /**
   * Whether the view is currently forcing every fold open for this request
   * (find.open, or an active target/notification pin). While false, no token
   * is published — there is nothing committed to reveal.
   */
  readonly revealAll: boolean;
  /**
   * Identifies the current reveal request (see `revealKey` above). Changing
   * this while `revealAll` stays true starts a new generation once the new
   * request's DOM commits (e.g. re-arming a repeated notification replay).
   */
  readonly revealKey: string;
  /**
   * Value that changes whenever the DOM this reveal request controls has
   * actually changed shape and needs to be re-walked once committed — e.g.
   * flattened item count, the resolved-expanded set size, or the manually-
   * revealed thread count. The hook re-publishes a fresh generation any time
   * this changes while `revealAll` is true.
   */
  readonly contentKey: unknown;
}

export interface UseIssueDisclosureRevealResult {
  /**
   * The latest committed token, or null when nothing has committed yet for
   * the current `issueId`/`revealKey` (including while `revealAll` is
   * false — closing find or releasing a target pin invalidates the token
   * immediately, it is not merely stale).
   */
  token: DisclosureRevealToken | null;
  /**
   * True when `candidate` names a issueId/revealKey pair that has an actual
   * committed generation published for it right now. Consumers should treat
   * any other state (no token yet, a stale issueId/revealKey, revealAll
   * false) as "do not walk the DOM yet."
   */
  isCommitted: (issueId: string, revealKey: string) => boolean;
}

/**
 * Publish a committed `(issueId, revealKey, generation)` token once the DOM
 * for the current reveal request has actually landed. Must be called from
 * the same component that renders the force-revealed content, since the
 * layout effect ordering guarantee only holds for effects scheduled by that
 * commit.
 */
export function useIssueDisclosureReveal(
  options: UseIssueDisclosureRevealOptions,
): UseIssueDisclosureRevealResult {
  const { issueId, revealAll, revealKey, contentKey } = options;

  const [token, setToken] = useState<DisclosureRevealToken | null>(null);
  const generationRef = useRef(0);

  // Layout effect: runs synchronously after DOM mutations commit but before
  // paint, so by the time this callback fires, every force-opened fold that
  // this render produced is guaranteed to be in the DOM. Never a rAF/timer —
  // this is the "committed" evidence 01-DESIGN requires, not a guess at when
  // the browser probably finished laying things out. (Content that is still
  // asynchronously reflowing, e.g. streamed markdown, is covered separately
  // by use-in-page-find's own MutationObserver re-collection, gated by this
  // same token so it never runs ahead of it.)
  useLayoutEffect(() => {
    if (!revealAll) {
      setToken(null);
      return;
    }
    generationRef.current += 1;
    setToken({ issueId, revealKey, generation: generationRef.current });
    // Invalidate immediately on cleanup (issue switch, revealKey change, or
    // revealAll flipping false before the next commit publishes a fresh
    // token) so a consumer holding the previous token's identity never
    // mistakes a stale generation for a current one.
    return () => setToken(null);
  }, [issueId, revealAll, revealKey, contentKey]);

  const isCommitted = (candidateIssueId: string, candidateRevealKey: string): boolean =>
    !!token && token.issueId === candidateIssueId && token.revealKey === candidateRevealKey;

  return { token, isCommitted };
}
