import { useEffect } from "react";

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
 * `scrollEl` scrolls, a transparent overlay inside it takes the pointer; wheel
 * events still bubble to the scroller. An overlay, not `pointer-events: none`
 * on the list: that property inherits, so toggling it re-styled every row on
 * each scroll burst (about 20 s of style recalc over one 300-row pass at 4x CPU).
 * Done on the DOM, not through state, so scrolling never re-renders the list.
 */
export function useScrollPointerShield(scrollEl: HTMLElement | null) {
  useEffect(() => {
    if (!scrollEl) return;
    let shield: HTMLDivElement | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const remove = () => {
      shield?.remove();
      shield = null;
    };
    const onScroll = () => {
      if (!shield) {
        const r = scrollEl.getBoundingClientRect();
        shield = document.createElement("div");
        shield.dataset.scrollShield = "";
        shield.style.cssText = `position:fixed;z-index:1;left:${r.left}px;top:${r.top}px;width:${r.width}px;height:${r.height}px`;
        scrollEl.appendChild(shield);
      }
      clearTimeout(timer);
      timer = setTimeout(remove, SETTLE_MS);
    };
    scrollEl.addEventListener("scroll", onScroll, { passive: true });
    return () => {
      scrollEl.removeEventListener("scroll", onScroll);
      clearTimeout(timer);
      remove();
    };
  }, [scrollEl]);
}
