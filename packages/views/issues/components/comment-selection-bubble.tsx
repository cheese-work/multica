"use client";

import { useEffect, useRef, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { autoUpdate, computePosition, flip, hide, offset, shift } from "@floating-ui/dom";

/**
 * Touch browsers (iOS) put their own selection menu above the selection, so the
 * selection action goes below it. It flips above only when there is no room below, and only
 * shifts horizontally (shift's default), never vertically, so it cannot cover the selected text.
 */
const TOUCH_GAP_BELOW = 28;
const TOUCH_GAP_ABOVE = 88;
/**
 * Measured on a real iPhone: the end-handle knob hangs ~20px below the selection, and the native
 * menu spans 23..78px above it. Below clears the knob; above (no room below) clears the menu.
 */
const touchGap = (placement: string) => (placement.startsWith("top") ? TOUCH_GAP_ABOVE : TOUCH_GAP_BELOW);
function placementFor(marker: boolean, touch: boolean) {
  if (marker) return "right-start";
  return touch ? "bottom" : "top";
}
const isTouch = () => !!window.matchMedia?.("(pointer: coarse)").matches;

/** Match the description's EditorBubbleMenu geometry for readonly selections. */
export function CommentSelectionBubble({ range, source, children, owner, markerRanges, onPositioned, portalTarget, keepInView = false }: {
  range: Range;
  source: HTMLElement;
  children: ReactNode;
  owner: string;
  markerRanges?: Range[];
  /** Called after the popup has a visible, usable position. */
  onPositioned?: () => void;
  /** Where the overlay lives in the DOM. Defaults to the page body. */
  portalTarget?: HTMLElement;
  keepInView?: boolean;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const marker = !!markerRanges;
  useEffect(() => {
    const element = ref.current;
    if (!element) return;
    let active = true;
    const anchor = {
      contextElement: source,
      getBoundingClientRect: () => range.getBoundingClientRect(),
    };
    let revision = 0;
    const update = () => {
      const currentRevision = ++revision;
      const touch = isTouch();
      const viewport = window.visualViewport;
      element.style.maxHeight = keepInView && viewport ? `${Math.max(0, viewport.height - 16)}px` : "";
      // Keep visibility tied to the real quote, not a zero-width point outside
      // the scroll container. Markers move horizontally but never clamp/flip
      // vertically: doing so piles unrelated annotations at the viewport edge.
      computePosition(anchor, element, {
        strategy: "fixed",
        placement: placementFor(marker, touch),
        middleware: markerRanges ? [
          offset(({ rects }) => {
            const sorted = markerRanges.map((item) => ({ item, top: item.getBoundingClientRect().top }))
              .sort((a, b) => a.top - b.top);
            let top = -Infinity;
            for (const item of sorted) {
              top = Math.max(top + 28, item.top);
              if (item.item === range) break;
            }
            return {
              mainAxis: source.getBoundingClientRect().right - (rects.reference.x + rects.reference.width) + 4,
              crossAxis: top - rects.reference.y,
            };
          }),
          shift({ padding: 8, mainAxis: false, crossAxis: true }), hide(),
        ] : [offset(touch ? ({ placement }) => touchGap(placement) : 8), flip({ padding: 8, altBoundary: true }), shift({ padding: 8, altBoundary: true, crossAxis: keepInView }), hide()],
      }).then(({ x, y, middlewareData }) => {
        if (!active || currentRevision !== revision || !element.isConnected) return;
        const rect = range.getBoundingClientRect();
        element.style.visibility = !source.isConnected || !range.startContainer.isConnected ||
          (rect.width === 0 && rect.height === 0) || (!keepInView && middlewareData.hide?.referenceHidden) ? "hidden" : "visible";
        element.style.left = `${x}px`;
        element.style.top = `${y}px`;
        if (element.style.visibility === "visible") onPositioned?.();
      });
    };
    // Sidebar transitions and font/content reflow can move a Range without
    // resizing its source element or triggering a window resize.
    const cleanup = autoUpdate(anchor, element, update, { animationFrame: true });
    return () => { active = false; cleanup(); };
  }, [range, source, marker, markerRanges, onPositioned, keepInView]);

  return createPortal(
    <div ref={ref} data-reply-annotation-overlay={owner}
      style={{ position: "fixed", zIndex: marker ? 20 : 50, visibility: "hidden", width: "max-content", overflowY: keepInView ? "auto" : undefined }}>
      {children}
    </div>,
    portalTarget ?? document.body,
  );
}
