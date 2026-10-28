import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from "react";
import { useAuthStore } from "@multica/core/auth";
import type { RowRangeSource } from "../issues/components/thread-minimap";

// Same rule `SelfAwareMention` renders by: a member mention of the viewer, or
// `@all`. Mentions shown as code are not links, so they are dropped first.
const CODE = /```[\s\S]*?```|`[^`\n]*`/g;

export function contentMentionsViewer(content: string | undefined, viewerId: string | undefined) {
  if (!content || !viewerId) return false;
  const prose = content.replace(CODE, " ");
  return prose.includes(`](mention://member/${viewerId})`) || /\]\(mention:\/\/all\//.test(prose);
}

const subscribeToAuth = (onChange: () => void) => useAuthStore.subscribe?.(onChange) ?? (() => {});

/** The signed-in user's id; undefined before the auth store is registered, unlike `useAuthStore(selector)`. */
export function useViewerId() {
  return useSyncExternalStore(
    subscribeToAuth,
    () => useAuthStore.getState?.().user?.id,
    () => undefined,
  );
}

/** Where on the container the jumped-to mention lands, as a share of its height. */
const LANDING_RATIO = 1 / 3;

interface Options {
  /** False while the list is closing or hidden: nothing is measured. */
  enabled?: boolean;
  /** The element that scrolls. */
  container: HTMLElement | null;
  /** Per list row, in order: does its content mention the viewer. */
  flags: readonly boolean[];
  /** Rows Virtuoso has mounted. Rows past its end exist only in `flags`. */
  range: RowRangeSource;
  /** Virtuoso reports absolute indexes; this is the `firstItemIndex` to subtract. */
  indexOffset?: number;
  /** Pixels at the bottom of the container that cover content (a sticky composer). */
  bottomInset?: () => number;
  /** Mounts and scrolls to a row that is not in the DOM. */
  scrollToIndex: (index: number) => void;
}

function pending({ container, flags, range, indexOffset = 0, bottomInset }: Options) {
  if (!container) return { elements: [] as HTMLElement[], unmounted: [] as number[] };
  const visibleBottom = container.getBoundingClientRect().bottom - (bottomInset?.() ?? 0);
  // Rendered mentions are measured, not guessed from row indexes: a long comment
  // can hold its mention far below the row's top.
  const elements = [...container.querySelectorAll<HTMLElement>(".mention-self")]
    .filter((el) => el.getBoundingClientRect().top >= visibleBottom)
    .sort((a, b) => a.getBoundingClientRect().top - b.getBoundingClientRect().top);
  const end = range.get()?.end;
  const unmounted: number[] = [];
  if (end !== undefined) {
    for (let i = end - indexOffset + 1; i < flags.length; i++) if (flags[i]) unmounted.push(i);
  }
  return { elements, unmounted };
}

/**
 * Counts the viewer's mentions below the visible part of a scroll container and
 * jumps to the next one. Mounted rows are read from the DOM, unmounted ones from
 * `flags`; a Virtuoso list only mounts the window around the viewport.
 */
export function useNextSelfMention(options: Options) {
  const { container, flags, range, enabled = true } = options;
  const latest = useRef(options);
  latest.current = options;
  const [count, setCount] = useState(0);

  useEffect(() => {
    if (!container || !enabled) return;
    let frame = 0;
    const measure = () => {
      frame = 0;
      const { elements, unmounted } = pending(latest.current);
      setCount(elements.length + unmounted.length);
    };
    const schedule = () => {
      if (!frame) frame = requestAnimationFrame(measure);
    };
    schedule();
    container.addEventListener("scroll", schedule, { passive: true });
    window.addEventListener("resize", schedule);
    const unsubscribe = range.subscribe(schedule);
    // Rows mount, measure and fold without a scroll event.
    const mutations = new MutationObserver(schedule);
    mutations.observe(container, { childList: true, subtree: true });
    return () => {
      if (frame) cancelAnimationFrame(frame);
      container.removeEventListener("scroll", schedule);
      window.removeEventListener("resize", schedule);
      unsubscribe();
      mutations.disconnect();
    };
  }, [container, range, flags, enabled]);

  const jump = useCallback(() => {
    const { container: el, scrollToIndex } = latest.current;
    if (!el) return;
    const box = el.getBoundingClientRect();
    const { elements, unmounted } = pending(latest.current);
    const target = elements[0];
    if (target) {
      const offset = target.getBoundingClientRect().top - box.top;
      el.scrollTo({
        top: el.scrollTop + offset - box.height * LANDING_RATIO,
        behavior: window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth",
      });
    } else if (unmounted[0] !== undefined) {
      scrollToIndex(unmounted[0]);
    }
  }, []);

  return { count, jump };
}
