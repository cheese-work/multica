import { useEffect, type RefObject } from "react";

/** Pointer events return this long after the last scroll event. */
const SETTLE_MS = 150;

/** Nearest ancestor that scrolls vertically, or null when none does. */
export function findScrollParent(el: HTMLElement): HTMLElement | null {
  for (let p = el.parentElement; p; p = p.parentElement) {
    const { overflowY } = getComputedStyle(p);
    if (overflowY === "auto" || overflowY === "scroll") return p;
  }
  return null;
}

/**
 * Rows slide under a still cursor while the list scrolls, and each one fires
 * `mouseenter`, which opens avatar hover cards and their queries. While
 * `scrollEl` scrolls, `target` ignores the pointer; the scroller itself still
 * takes the wheel. Toggled on the element, not through state, so scrolling
 * never re-renders the list.
 */
export function useScrollPointerShield(
  target: RefObject<HTMLElement | null>,
  scrollEl: HTMLElement | null,
) {
  useEffect(() => {
    const el = target.current;
    if (!scrollEl || !el) return;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const onScroll = () => {
      el.style.pointerEvents = "none";
      clearTimeout(timer);
      timer = setTimeout(() => {
        el.style.pointerEvents = "";
      }, SETTLE_MS);
    };
    scrollEl.addEventListener("scroll", onScroll, { passive: true });
    return () => {
      scrollEl.removeEventListener("scroll", onScroll);
      clearTimeout(timer);
      el.style.pointerEvents = "";
    };
  }, [target, scrollEl]);
}
