"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { ArrowDown } from "lucide-react";
import { useAuthStore } from "@multica/core/auth";
import { useT } from "../i18n";

// Floating "next mention of me" jump for long virtualized lists (issue thread,
// DM). Detection reads the data, not the DOM: Virtuoso only mounts the visible
// window, so a mention further down has no element to find. Only "where is the
// viewport" is read from the DOM, off the row index Virtuoso stamps on every
// mounted row.

const FENCED_CODE = /```[\s\S]*?```|`[^`\n]*`/g;

/** Whether markdown mentions the viewer: their member mention, or `@all`. */
export function mentionsViewer(markdown: string | null | undefined, viewerId: string | undefined) {
  if (!markdown || !viewerId) return false;
  const text = markdown.replace(FENCED_CODE, "");
  return text.includes(`](mention://member/${viewerId})`) || text.includes("](mention://all/");
}

/** Hook form: a predicate over markdown for the signed-in viewer. */
export function useMentionsViewer() {
  const viewerId = useAuthStore((s) => s.user?.id);
  return useCallback((markdown: string | null | undefined) => mentionsViewer(markdown, viewerId), [viewerId]);
}

/** Highest row index with any part inside the scroller's viewport, if any row is mounted. */
function lastVisibleRow(scrollEl: HTMLElement): number | null {
  const view = scrollEl.getBoundingClientRect();
  let last: number | null = null;
  for (const row of scrollEl.querySelectorAll<HTMLElement>("[data-item-index]")) {
    const rect = row.getBoundingClientRect();
    if (rect.bottom <= view.top || rect.top >= view.bottom) continue;
    const index = Number(row.dataset.itemIndex);
    if (last === null || index > last) last = index;
  }
  return last;
}

interface NextSelfMentionProps {
  scrollEl: HTMLElement | null;
  /** Per row, in data order: does the row mention the viewer. */
  flags: readonly boolean[];
  /** Virtuoso's `firstItemIndex`, when the list prepends older rows. */
  firstItemIndex?: number;
  /** Scroll the row at this data index into view. */
  onJump: (index: number) => void;
  className?: string;
}

export function NextSelfMention({ scrollEl, flags, firstItemIndex = 0, onJump, className }: NextSelfMentionProps) {
  const { t } = useT("editor");
  const [visibleEnd, setVisibleEnd] = useState<number | null>(null);

  useEffect(() => {
    if (!scrollEl) return;
    let frame = 0;
    const measure = () => {
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => setVisibleEnd(lastVisibleRow(scrollEl)));
    };
    measure();
    scrollEl.addEventListener("scroll", measure, { passive: true });
    const resize = new ResizeObserver(measure);
    resize.observe(scrollEl);
    // Rows mount and unmount as Virtuoso windows the list.
    const mutations = new MutationObserver(measure);
    mutations.observe(scrollEl, { childList: true, subtree: true });
    return () => {
      cancelAnimationFrame(frame);
      scrollEl.removeEventListener("scroll", measure);
      resize.disconnect();
      mutations.disconnect();
    };
  }, [scrollEl, flags, firstItemIndex]);

  const below = useMemo(() => {
    if (visibleEnd === null) return [];
    const out: number[] = [];
    for (let i = visibleEnd - firstItemIndex + 1; i < flags.length; i++) if (flags[i]) out.push(i);
    return out;
  }, [flags, visibleEnd, firstItemIndex]);

  if (below.length === 0) return null;
  return (
    <button
      type="button"
      onClick={() => onJump(below[0]!)}
      className={
        className ??
        "absolute bottom-28 right-6 z-20 inline-flex items-center gap-1.5 rounded-full bg-primary px-3 py-1.5 text-caption font-medium text-primary-foreground shadow-md outline-none hover:bg-primary/90 focus-visible:ring-2 focus-visible:ring-ring"
      }
    >
      <ArrowDown className="size-3.5" aria-hidden />
      {t(($) => $.next_self_mention.label)}
      {below.length > 1 && (
        <span data-testid="next-self-mention-count" className="rounded-full bg-primary-foreground/20 px-1.5">
          {below.length}
        </span>
      )}
    </button>
  );
}
