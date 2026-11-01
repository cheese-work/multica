"use client";

import { useCallback, useEffect, useRef, useState, type KeyboardEvent, type PointerEvent, type RefObject } from "react";
import { centerOn, fitView, intersects, modeFor, panBy, zoomAt, ZOOM_SPLIT, type Box, type Layout, type Mode, type View } from "./hierarchy-layout";

const KEY_PAN = 80;
const ZOOM_STEP = 1.25;
const DRAG_THRESHOLD = 4;
const WHEEL_SENSITIVITY = 0.0015;
type Size = { w: number; h: number };

/** Whole hierarchy in view. The mode follows from the scale and the scale from the mode, so try cards first. */
const fitAll = (all: Record<Mode, Layout>, viewport: Size): View => {
  const cards = fitView(all.cards.bounds, viewport);
  if (modeFor(cards.scale) === "cards") return cards;
  const groups = fitView(all.groups.bounds, viewport);
  return modeFor(groups.scale) === "groups" ? groups : centerOn(all.groups.bounds, viewport, ZOOM_SPLIT - 0.01);
};

const currentBox = (layout: Layout) => layout.items.find((item) => item.kind === "issue" && item.role === "current")!.box;

/**
 * Pan / zoom state of the canvas. Wheel, drag and key input move the view
 * directly (no transition, so they track the pointer); the buttons animate.
 */
export function useCanvasView(layouts: Record<Mode, Layout>, viewportRef: RefObject<HTMLDivElement | null>) {
  const [size, setSize] = useState<Size | null>(null);
  const [view, setView] = useState<View>({ x: 0, y: 0, scale: 1 });
  const [smooth, setSmooth] = useState(false);
  const layoutsRef = useRef(layouts);
  layoutsRef.current = layouts;
  const sizeRef = useRef<Size | null>(null);
  sizeRef.current = size;
  const drag = useRef<{ x: number; y: number; active: boolean } | null>(null);
  const dragged = useRef(false);
  const mode = modeFor(view.scale);

  /** Groups and cards place the current card differently; keep it fixed on screen when the mode flips. */
  const move = useCallback((next: (prev: View) => View, animate: boolean) => {
    setSmooth(animate);
    setView((prev) => {
      const target = next(prev);
      const [from, to] = [modeFor(prev.scale), modeFor(target.scale)];
      if (from === to) return target;
      const a = currentBox(layoutsRef.current[from]);
      const b = currentBox(layoutsRef.current[to]);
      return { ...target, x: target.x + (a.x - b.x) * target.scale, y: target.y + (a.y - b.y) * target.scale };
    });
  }, []);

  // Absolute targets (Fit, Me) are computed for the layout they land in.
  const jump = useCallback((target: (all: Record<Mode, Layout>, size: Size) => View) => {
    if (!sizeRef.current) return;
    setSmooth(true);
    setView(target(layoutsRef.current, sizeRef.current));
  }, []);

  const fit = useCallback(() => jump(fitAll), [jump]);
  const me = useCallback(() => jump((all, vp) => centerOn(currentBox(all.cards), vp, 1)), [jump]);

  const zoomBy = useCallback((factor: number, anchor?: { x: number; y: number }, animate = true) => {
    move((prev) => {
      const vp = sizeRef.current ?? { w: 0, h: 0 };
      return zoomAt(prev, prev.scale * factor, anchor ?? { x: vp.w / 2, y: vp.h / 2 });
    }, animate);
  }, [move]);

  useEffect(() => {
    const el = viewportRef.current;
    if (!el) return;
    const measure = () => {
      const { width, height } = el.getBoundingClientRect();
      if (width > 0 && height > 0) setSize((prev) => (prev && prev.w === width && prev.h === height ? prev : { w: width, h: height }));
    };
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, [viewportRef]);

  const fitted = useRef(false);
  useEffect(() => {
    if (!size || fitted.current) return;
    fitted.current = true;
    setView(fitAll(layoutsRef.current, size));
  }, [size]);

  // React attaches wheel listeners as passive, so preventing the page scroll needs a native one.
  useEffect(() => {
    const el = viewportRef.current;
    if (!el) return;
    const onWheel = (event: WheelEvent) => {
      event.preventDefault();
      const rect = el.getBoundingClientRect();
      zoomBy(Math.exp(-event.deltaY * WHEEL_SENSITIVITY), { x: event.clientX - rect.left, y: event.clientY - rect.top }, false);
    };
    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
  }, [viewportRef, zoomBy]);

  const visible: Box | null = size ? { x: -view.x / view.scale, y: -view.y / view.scale, w: size.w / view.scale, h: size.h / view.scale } : null;

  /** Tab can focus a card outside the viewport; bring it in rather than let the browser scroll the clipped container. */
  const reveal = useCallback((box: Box) => {
    if (!sizeRef.current) return;
    setView((prev) => {
      const vp = sizeRef.current!;
      const seen = { x: -prev.x / prev.scale, y: -prev.y / prev.scale, w: vp.w / prev.scale, h: vp.h / prev.scale };
      return intersects(box, seen) && box.x >= seen.x && box.y >= seen.y && box.x + box.w <= seen.x + seen.w && box.y + box.h <= seen.y + seen.h
        ? prev
        : centerOn(box, vp, prev.scale);
    });
    setSmooth(true);
  }, []);

  const bind = {
    onPointerDown: (event: PointerEvent<HTMLDivElement>) => {
      if (event.button !== 0) return;
      drag.current = { x: event.clientX, y: event.clientY, active: false };
    },
    onPointerMove: (event: PointerEvent<HTMLDivElement>) => {
      const start = drag.current;
      if (!start || event.buttons !== 1) return;
      if (!start.active) {
        if (Math.hypot(event.clientX - start.x, event.clientY - start.y) < DRAG_THRESHOLD) return;
        // Capture only once it is a drag: capturing on press would retarget the click away from the card.
        start.active = true;
        dragged.current = true;
        event.currentTarget.setPointerCapture?.(event.pointerId);
      }
      const [dx, dy] = [event.clientX - start.x, event.clientY - start.y];
      [start.x, start.y] = [event.clientX, event.clientY];
      move((prev) => panBy(prev, dx, dy), false);
    },
    onPointerUp: (event: PointerEvent<HTMLDivElement>) => {
      if (drag.current?.active) event.currentTarget.releasePointerCapture?.(event.pointerId);
      drag.current = null;
      setTimeout(() => { dragged.current = false; }, 0);
    },
    onClickCapture: (event: React.MouseEvent) => {
      if (!dragged.current) return;
      event.preventDefault();
      event.stopPropagation();
    },
    onKeyDown: (event: KeyboardEvent<HTMLDivElement>) => {
      const pan: Record<string, [number, number]> = { ArrowLeft: [KEY_PAN, 0], ArrowRight: [-KEY_PAN, 0], ArrowUp: [0, KEY_PAN], ArrowDown: [0, -KEY_PAN] };
      const delta = pan[event.key];
      if (delta) move((prev) => panBy(prev, delta[0], delta[1]), true);
      else if (event.key === "+" || event.key === "=") zoomBy(ZOOM_STEP);
      else if (event.key === "-" || event.key === "_") zoomBy(1 / ZOOM_STEP);
      else return;
      event.preventDefault();
    },
    onScroll: (event: React.UIEvent<HTMLDivElement>) => event.currentTarget.scrollTo(0, 0),
  };

  return { view, mode, layout: layouts[mode], size, smooth, visible, fit, me, zoomIn: () => zoomBy(ZOOM_STEP), zoomOut: () => zoomBy(1 / ZOOM_STEP), zoomBy, reveal, bind };
}
