"use client";

import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { ArrowDown } from "lucide-react";
import { fromMarkdown } from "mdast-util-from-markdown";
import { visit } from "unist-util-visit";
import { useAuthStore } from "@multica/core/auth";
import { preprocessMentionShortcodes } from "@multica/ui/markdown";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../i18n";

// Floating "next mention of me" jump for long virtualized lists (issue thread,
// DM). Detection reads the data, not the DOM: Virtuoso only mounts the visible
// window, so a mention further down has no element to find. Only "where is the
// viewport" is read from the DOM, off the row index Virtuoso stamps on every
// mounted row.

// Virtuoso scrolls to an estimated offset for rows it has not measured, so one
// jump can land short or long. Re-issue it each frame until the row sits at the
// top of the viewport, for at most ~1s, and give up the moment the reader scrolls.
const SETTLE_TOLERANCE_PX = 40;
const SETTLE_MAX_FRAMES = 60;

/**
 * Whether markdown mentions the viewer: their member mention, or `@all`.
 * Parses the markdown, so only link nodes count — code of any kind (inline,
 * backtick or tilde fence, indented) never does. Legacy `[@ id=".." label=".."]`
 * shortcodes are normalized first, as the renderer does.
 */
export function mentionsViewer(markdown: string | null | undefined, viewerId: string | undefined) {
  if (!markdown || !viewerId) return false;
  let found = false;
  visit(fromMarkdown(preprocessMentionShortcodes(markdown)), "link", (node) => {
    if (node.url === `mention://member/${viewerId}` || node.url.startsWith("mention://all/")) found = true;
  });
  return found;
}

function subscribeToAuth(onChange: () => void) {
  return useAuthStore.subscribe?.(onChange) ?? (() => {});
}

/**
 * Hook form: a predicate over markdown for the signed-in viewer. Reads the
 * store like RichContent does, so a host without a registered auth store
 * (tests, previews) just sees no viewer.
 */
export function useMentionsViewer() {
  const viewerId = useSyncExternalStore(
    subscribeToAuth,
    () => useAuthStore.getState?.().user?.id,
    () => undefined,
  );
  return useCallback((markdown: string | null | undefined) => mentionsViewer(markdown, viewerId), [viewerId]);
}

/**
 * Highest row index at or above the viewport's bottom edge, if any row is
 * mounted. With no row on screen (a long description can push every row below
 * it) that is the row just before the first mounted one, or the last mounted
 * row when all of them sit above the viewport.
 */
function lastVisibleRow(scrollEl: HTMLElement): number | null {
  const view = scrollEl.getBoundingClientRect();
  let visible: number | null = null;
  let lastAbove: number | null = null;
  let firstBelow: number | null = null;
  for (const row of scrollEl.querySelectorAll<HTMLElement>("[data-item-index]")) {
    const rect = row.getBoundingClientRect();
    const index = Number(row.dataset.itemIndex);
    if (rect.bottom <= view.top) {
      if (lastAbove === null || index > lastAbove) lastAbove = index;
    } else if (rect.top >= view.bottom) {
      if (firstBelow === null || index < firstBelow) firstBelow = index;
    } else if (visible === null || index > visible) visible = index;
  }
  if (visible !== null) return visible;
  return firstBelow !== null ? firstBelow - 1 : lastAbove;
}

interface NextSelfMentionProps {
  scrollEl: HTMLElement | null;
  /** Per row, in data order: does the row mention the viewer. */
  flags: readonly boolean[];
  /** Virtuoso's `firstItemIndex`, when the list prepends older rows. */
  firstItemIndex?: number;
  /** Scroll the row at this data index into view. */
  onJump: (index: number) => void;
  /** Off while the host list is closing: no measuring, nothing rendered. Default on. */
  enabled?: boolean;
  /** Placement utilities; the default floats above a composer that lives inside the scroller. */
  positionClassName?: string;
}

export function NextSelfMention({ scrollEl, flags, firstItemIndex = 0, onJump, enabled = true, positionClassName = "absolute bottom-28 right-6 z-20" }: NextSelfMentionProps) {
  const { t } = useT("editor");
  const [visibleEnd, setVisibleEnd] = useState<number | null>(null);
  const jumpId = useRef(0);

  useEffect(() => {
    if (!scrollEl || !enabled) return;
    let frame = 0;
    const measure = () => {
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => setVisibleEnd(lastVisibleRow(scrollEl)));
    };
    measure();
    scrollEl.addEventListener("scroll", measure, { passive: true });
    const cancelJump = () => void jumpId.current++;
    scrollEl.addEventListener("wheel", cancelJump, { passive: true });
    scrollEl.addEventListener("touchstart", cancelJump, { passive: true });
    const resize = new ResizeObserver(measure);
    resize.observe(scrollEl);
    // Rows mount and unmount as Virtuoso windows the list.
    const mutations = new MutationObserver(measure);
    mutations.observe(scrollEl, { childList: true, subtree: true });
    return () => {
      cancelAnimationFrame(frame);
      scrollEl.removeEventListener("scroll", measure);
      scrollEl.removeEventListener("wheel", cancelJump);
      scrollEl.removeEventListener("touchstart", cancelJump);
      cancelJump();
      resize.disconnect();
      mutations.disconnect();
    };
  }, [scrollEl, enabled, flags, firstItemIndex]);

  const below = useMemo(() => {
    if (visibleEnd === null) return [];
    const out: number[] = [];
    for (let i = visibleEnd - firstItemIndex + 1; i < flags.length; i++) if (flags[i]) out.push(i);
    return out;
  }, [flags, visibleEnd, firstItemIndex]);

  const jump = (target: number) => {
    const id = ++jumpId.current;
    let frames = 0;
    const settle = () => {
      if (!scrollEl || id !== jumpId.current || ++frames > SETTLE_MAX_FRAMES) return;
      const row = scrollEl.querySelector(`[data-item-index="${target + firstItemIndex}"]`);
      const offTop = row ? row.getBoundingClientRect().top - scrollEl.getBoundingClientRect().top : Infinity;
      if (Math.abs(offTop) <= SETTLE_TOLERANCE_PX) return;
      onJump(target);
      requestAnimationFrame(settle);
    };
    onJump(target);
    requestAnimationFrame(settle);
  };

  if (!enabled || below.length === 0) return null;
  return (
    <button
      type="button"
      onClick={() => jump(below[0]!)}
      className={cn(
        "inline-flex items-center gap-1.5 rounded-full bg-primary px-3 py-1.5 text-caption font-medium text-primary-foreground shadow-md outline-none hover:bg-primary/90 focus-visible:ring-2 focus-visible:ring-ring",
        positionClassName,
      )}
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
