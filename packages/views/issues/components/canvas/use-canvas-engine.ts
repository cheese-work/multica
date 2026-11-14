"use client";

import { useCallback, useEffect, useRef, useState, type KeyboardEvent, type PointerEvent, type RefObject } from "react";
import { centerOn, intersects, panBy, zoomAt, type Box, type View } from "./canvas-math";

const KEY_PAN = 80;
export const ZOOM_STEP = 1.25;
const DRAG_THRESHOLD = 4;
const WHEEL_SENSITIVITY = 0.0015;
export type Size = { w: number; h: number };

const REDUCED_MOTION = "(prefers-reduced-motion: reduce)";

/** Live `prefers-reduced-motion` preference; false where `matchMedia` is missing. */
function usePrefersReducedMotion() {
  const [reduced, setReduced] = useState(() => globalThis.matchMedia?.(REDUCED_MOTION).matches ?? false);
  useEffect(() => {
    const query = globalThis.matchMedia?.(REDUCED_MOTION);
    if (!query) return;
    const update = () => setReduced(query.matches);
    update();
    query.addEventListener?.("change", update);
    return () => query.removeEventListener?.("change", update);
  }, []);
  return reduced;
}

interface Options {
  viewportRef: RefObject<HTMLDivElement | null>;
  /** View applied once, when the viewport is first measured. */
  initialView: (size: Size) => View;
  /** Adjust a pan/zoom result before it lands; lets a consumer keep something fixed on screen. */
  adjust?: (prev: View, target: View) => View;
}

/**
 * Pan / zoom state of a canvas, independent of what is drawn on it. Wheel,
 * drag and key input move the view directly (no transition, so they track the
 * pointer); the buttons animate.
 */
export function useCanvasEngine({ viewportRef, initialView, adjust }: Options) {
  const [size, setSize] = useState<Size | null>(null);
  const [view, setView] = useState<View>({ x: 0, y: 0, scale: 1 });
  const [animate, setSmooth] = useState(false);
  const reducedMotion = usePrefersReducedMotion();
  const smooth = animate && !reducedMotion;
  const sizeRef = useRef<Size | null>(null);
  sizeRef.current = size;
  const initialViewRef = useRef(initialView);
  initialViewRef.current = initialView;
  const adjustRef = useRef(adjust);
  adjustRef.current = adjust;
  const drag = useRef<{ x: number; y: number; active: boolean } | null>(null);
  const dragged = useRef(false);

  const move = useCallback((next: (prev: View) => View, animate: boolean) => {
    setSmooth(animate);
    setView((prev) => {
      const target = next(prev);
      return adjustRef.current ? adjustRef.current(prev, target) : target;
    });
  }, []);

  /** Absolute targets (Fit, Me) are computed from the viewport size. */
  const jump = useCallback((target: (size: Size) => View) => {
    if (!sizeRef.current) return;
    setSmooth(true);
    setView(target(sizeRef.current));
  }, []);

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
    setView(initialViewRef.current(size));
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

  return { view, size, smooth, visible, move, jump, zoomBy, zoomIn: () => zoomBy(ZOOM_STEP), zoomOut: () => zoomBy(1 / ZOOM_STEP), fit: () => jump(initialViewRef.current), reveal, bind };
}
