import { useEffect, useRef } from "react";
import { useRestoredViewState, useViewStateWriter } from "../../platform";

/**
 * Where the reader was in a virtualized issue timeline: the top visible row
 * and how many px of it are scrolled out above the viewport. A pixel offset
 * cannot return there — row heights are estimated until measured, so the same
 * offset lands on a different comment after a remount.
 */
export interface ScrollAnchor {
  key: string;
  offset: number;
}

export function issueScrollAnchorMementoKey(issueId: string): string {
  return `scroll-anchor:${issueId}`;
}

/**
 * The top visible timeline row, or null while the reader is above the list
 * (header, description) — there the pixel restore is exact.
 */
export function readScrollAnchor(
  container: HTMLElement,
  keyAt: (index: number) => string | undefined,
): ScrollAnchor | null {
  const containerTop = container.getBoundingClientRect().top;
  for (const row of container.querySelectorAll<HTMLElement>("[data-item-index]")) {
    const rect = row.getBoundingClientRect();
    if (rect.bottom <= containerTop) continue;
    const index = Number(row.dataset.itemIndex);
    const key = keyAt(index);
    if (key === undefined || (index === 0 && rect.top > containerTop)) return null;
    return { key, offset: Math.max(0, Math.round(containerTop - rect.top)) };
  }
  return null;
}

function parseScrollAnchor(raw: string | undefined): ScrollAnchor | null {
  if (!raw) return null;
  try {
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== "object" || parsed === null) return null;
    const { key, offset } = parsed as Partial<ScrollAnchor>;
    return typeof key === "string" && typeof offset === "number" ? { key, offset } : null;
  } catch {
    return null;
  }
}

/**
 * Records the anchor while the reader scrolls and returns the one saved for
 * this issue before it was last left. The returned value is frozen per issue
 * at first render, so the recording below never changes what is restored.
 *
 * Recording is scroll-driven only: by unmount the router has already moved
 * `location.pathname`, which keys the web adapter's view state.
 */
export function useIssueDetailScrollAnchor({
  issueId,
  scrollContainerEl,
  ready,
  disabled = false,
  keyAt,
}: {
  issueId: string;
  scrollContainerEl: HTMLElement | null;
  ready: boolean;
  /** A peek always opens at the top: it neither records nor restores. */
  disabled?: boolean;
  keyAt: (index: number) => string | undefined;
}): ScrollAnchor | null {
  const mementoKey = issueScrollAnchorMementoKey(issueId);
  const saved = useRestoredViewState(mementoKey);
  const write = useViewStateWriter();
  const initial = useRef<{ issueId: string; anchor: ScrollAnchor | null } | null>(null);
  if (initial.current?.issueId !== issueId) {
    initial.current = { issueId, anchor: disabled ? null : parseScrollAnchor(saved) };
  }

  const keyAtRef = useRef(keyAt);
  keyAtRef.current = keyAt;

  useEffect(() => {
    if (!scrollContainerEl || !ready || disabled) return;
    let frame = 0;
    const save = () => {
      frame = 0;
      const anchor = readScrollAnchor(scrollContainerEl, keyAtRef.current);
      write(mementoKey, anchor ? JSON.stringify(anchor) : undefined);
    };
    const onScroll = () => {
      if (!frame) frame = requestAnimationFrame(save);
    };
    scrollContainerEl.addEventListener("scroll", onScroll, { passive: true });
    return () => {
      scrollContainerEl.removeEventListener("scroll", onScroll);
      cancelAnimationFrame(frame);
    };
  }, [scrollContainerEl, ready, disabled, write, mementoKey]);

  return initial.current.anchor;
}
