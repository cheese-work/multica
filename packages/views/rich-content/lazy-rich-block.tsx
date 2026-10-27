"use client";

/**
 * LazyRichBlock — near-viewport, stable-size mount gate for rich blocks
 * (MUL-4922 performance contract).
 *
 * A long chat session or a long comment thread can contain dozens of Mermaid
 * diagrams and sandboxed HTML iframes. Instantiating them all at once costs a
 * Mermaid render (async, layout-heavy) and an iframe (its own document +
 * script execution) per block, whether or not the user ever scrolls to them.
 * This shell defers each rich leaf until it is near the viewport.
 *
 * Two properties make this safe inside Virtuoso's virtualized list:
 *
 * 1. STABLE SIZE. The shell reserves the block's expected height BEFORE mounting
 *    and keeps that reservation as a `min-height` afterwards. Without it, a
 *    block would measure 0px while off-screen and jump to its real height on
 *    mount — which is precisely the measurement churn that makes a virtualized
 *    list mis-estimate item sizes and lose scroll position. The reservation is
 *    not a guess local to this file: it comes from the leaf components
 *    themselves (a session-cached real diagram height when available, else their
 *    documented skeleton/iframe height), so a cache hit mounts with zero shift.
 *
 * 2. MOUNT-ONCE LATCH. Once mounted the block is never unmounted, even when
 *    scrolled far away. Unmounting would re-run Mermaid and rebuild the iframe
 *    on every pass, and would discard the viewer's pan/zoom state — trading a
 *    one-time cost for a repeated one. Memory is bounded by "blocks the user
 *    actually scrolled past", not by the whole transcript.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useRichContentScrollRoot } from "./scroll-root";
import {
  hasBlockMounted,
  markBlockMounted,
  mountedBlockKey,
  unmarkBlockMounted,
} from "./mounted-block-registry";

// Restoring an already-mounted block must happen before paint, or a recycled
// Virtuoso row would flash its placeholder on the way back into view.
// useLayoutEffect does not run on the server (and warns if called there), so the
// server takes the plain-effect branch — neither runs during SSR, so the first
// frame is unaffected either way.
const useIsomorphicLayoutEffect =
  typeof window !== "undefined" ? useLayoutEffect : useEffect;

/**
 * How far outside the viewport a block starts mounting. Sized to cover
 * Virtuoso's own overscan (`increaseViewportBy` is 400px top / 600px bottom in
 * the chat list) so a block is ready by the time it is scrolled into view,
 * without eagerly building the whole transcript.
 */
const NEAR_VIEWPORT_ROOT_MARGIN = "800px 0px";

/**
 * How far outside the viewport a mounted block must be before it can be
 * released: three times the mount margin, so a block is never released inside
 * the range where scrolling would bring it straight back.
 */
const FAR_VIEWPORT_ROOT_MARGIN = "2400px 0px";

/** Quiet period outside the far margin before a block is released. */
const RELEASE_AFTER_MS = 5_000;

const RichBlockHoldContext = createContext<((held: boolean) => void) | null>(null);

function supportsIntersectionObserver(): boolean {
  return typeof window !== "undefined" && typeof window.IntersectionObserver === "function";
}

/**
 * Keep the enclosing LazyRichBlock mounted while `active` is true. Used by a
 * block's own interactive state that lives outside the shell's DOM, such as a
 * viewer in a portal. Outside a LazyRichBlock it does nothing.
 */
export function useRichBlockHold(active: boolean): void {
  const setHeld = useContext(RichBlockHoldContext);
  useEffect(() => {
    if (!setHeld || !active) return;
    setHeld(true);
    return () => setHeld(false);
  }, [setHeld, active]);
}

export function LazyRichBlock({
  reservedHeightPx,
  sourceKey,
  children,
}: {
  /** Expected height of the mounted block; reserved before and after mount. */
  reservedHeightPx: number;
  /**
   * Stable identity for this block's content. Used to remember that it has
   * already been mounted, so a virtualized row that gets recycled and later
   * re-created does not pay the mount cost again.
   */
  sourceKey?: string;
  children: ReactNode;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const scrollRoot = useRichContentScrollRoot();
  const registryKey = sourceKey == null ? null : mountedBlockKey("rich", sourceKey);
  // ALWAYS false on the first render, on both server and client.
  //
  // Deriving this from feature detection (`typeof window`, IntersectionObserver
  // presence) would make the first frame environment-dependent: the server, with
  // no `window`, would render the full Mermaid/HTML subtree while the browser's
  // hydration pass renders a placeholder — a markup mismatch, and an SSR that
  // silently bypasses the lazy gate it is supposed to honour. `"use client"`
  // does not opt a component out of Next's server render, so the only safe
  // initial state is the one both environments can agree on.
  //
  // Everything environment-specific happens in the effect below, which never
  // runs on the server.
  const [mounted, setMounted] = useState(false);
  // Height of the last released build, kept so the placeholder holds the space.
  const [releasedHeightPx, setReleasedHeightPx] = useState(0);
  const holdCount = useRef(0);
  // The far observer has reported the shell outside the far margin.
  const farAway = useRef(false);
  const releaseTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Restore a block this session has already built. Kept out of the initial
  // state for the same reason as everything else here: the registry is empty on
  // the server, so seeding from it would make the first frame differ between
  // server and client.
  useIsomorphicLayoutEffect(() => {
    if (mounted || registryKey == null) return;
    if (hasBlockMounted(registryKey)) setMounted(true);
  }, [mounted, registryKey]);

  useEffect(() => {
    if (mounted) return;

    // No IntersectionObserver (jsdom, older webviews): fall back to mounting
    // eagerly. This runs in an effect rather than in the initial state so the
    // first committed frame still matches the server's.
    if (!supportsIntersectionObserver()) {
      setMounted(true);
      return;
    }

    const el = ref.current;
    if (!el) return;

    const observer = new IntersectionObserver(
      (entries) => {
        if (entries.some((entry) => entry.isIntersecting)) {
          // Latch: stop observing so the block never unmounts on scroll-away.
          setMounted(true);
          observer.disconnect();
        }
      },
      {
        // Clip against the surface's own scroll container when there is one.
        // With the default (viewport) root, rootMargin would expand the wrong
        // box and a Chat block would only load once already visible.
        root: scrollRoot,
        rootMargin: NEAR_VIEWPORT_ROOT_MARGIN,
      },
    );
    observer.observe(el);
    return () => observer.disconnect();
  }, [mounted, scrollRoot]);

  // Record the mount so a recycled row can skip straight to the built block.
  useEffect(() => {
    if (mounted && registryKey != null) markBlockMounted(registryKey);
  }, [mounted, registryKey]);

  const cancelRelease = useCallback(() => {
    if (releaseTimer.current == null) return;
    clearTimeout(releaseTimer.current);
    releaseTimer.current = null;
  }, []);

  // Read from the DOM, not tracked from focus events: a focused control that is
  // removed fires no blur, so a tracked flag could pin the block forever.
  const hasFocusWithin = useCallback(
    () => ref.current?.contains(document.activeElement) ?? false,
    [],
  );

  const release = useCallback(() => {
    releaseTimer.current = null;
    const el = ref.current;
    if (!el || hasFocusWithin() || holdCount.current > 0) return;
    setReleasedHeightPx(Math.ceil(el.getBoundingClientRect().height));
    if (registryKey != null) unmarkBlockMounted(registryKey);
    setMounted(false);
  }, [registryKey, hasFocusWithin]);

  // Release only once far away, quiet and not in use. Re-evaluated whenever one
  // of those three changes.
  const scheduleReleaseIfIdle = useCallback(() => {
    cancelRelease();
    if (!farAway.current || hasFocusWithin() || holdCount.current > 0) return;
    releaseTimer.current = setTimeout(release, RELEASE_AFTER_MS);
  }, [cancelRelease, release, hasFocusWithin]);

  const setHeld = useCallback(
    (held: boolean) => {
      holdCount.current += held ? 1 : -1;
      scheduleReleaseIfIdle();
    },
    [scheduleReleaseIfIdle],
  );

  useEffect(() => {
    if (!mounted || !supportsIntersectionObserver()) return;
    const el = ref.current;
    if (!el) return;

    farAway.current = false;
    const observer = new IntersectionObserver(
      (entries) => {
        // The last entry is the most recent state of the single observed shell.
        const latest = entries[entries.length - 1];
        if (!latest) return;
        farAway.current = !latest.isIntersecting;
        scheduleReleaseIfIdle();
      },
      { root: scrollRoot, rootMargin: FAR_VIEWPORT_ROOT_MARGIN },
    );
    observer.observe(el);
    return () => {
      observer.disconnect();
      farAway.current = false;
      cancelRelease();
    };
  }, [mounted, scrollRoot, scheduleReleaseIfIdle, cancelRelease]);

  return (
    <div
      ref={ref}
      data-rich-block-shell=""
      data-mounted={mounted ? "" : undefined}
      // The shell, not the block inside it, owns the space around the block
      // (MUL-7733). A child's bottom margin cannot collapse through a box with
      // a min-height, so on the block it sat inside the reservation: the gap
      // below grew by a paragraph margin and the page shifted on mount. Here
      // it collapses with the paragraphs on both sides, and the prose
      // first/last-child reset still applies.
      className="my-5"
      onFocus={scheduleReleaseIfIdle}
      onBlur={scheduleReleaseIfIdle}
      // The reservation persists after mount so the shell never shrinks back
      // and re-triggers a measurement pass. Real diagrams and the fixed-height
      // HTML preview normally exceed it, so it rarely adds visible space.
      style={{ minHeight: Math.max(reservedHeightPx, releasedHeightPx) }}
    >
      {mounted ? (
        <RichBlockHoldContext.Provider value={setHeld}>{children}</RichBlockHoldContext.Provider>
      ) : (
        <RichBlockPlaceholder />
      )}
    </div>
  );
}

/**
 * Pre-mount placeholder. Deliberately inert and unlabelled-as-loading: it fills
 * reserved space rather than announcing work, because nothing is loading yet —
 * the block simply has not been scrolled near.
 */
function RichBlockPlaceholder() {
  return (
    <div
      className="h-full w-full rounded-md border border-border/50 bg-muted/20"
      aria-hidden="true"
    />
  );
}
