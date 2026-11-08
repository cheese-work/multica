"use client";

import { useCallback, useRef, type RefObject } from "react";
import { centerOn, fitView, modeFor, ZOOM_SPLIT, type Layout, type Mode, type View } from "./hierarchy-layout";
import { useCanvasEngine, ZOOM_STEP, type Size } from "../canvas/use-canvas-engine";

/** Whole hierarchy in view. The mode follows from the scale and the scale from the mode, so try cards first. */
const fitAll = (all: Record<Mode, Layout>, viewport: Size): View => {
  const cards = fitView(all.cards.bounds, viewport);
  if (modeFor(cards.scale) === "cards") return cards;
  const groups = fitView(all.groups.bounds, viewport);
  return modeFor(groups.scale) === "groups" ? groups : centerOn(all.groups.bounds, viewport, ZOOM_SPLIT - 0.01);
};

const currentBox = (layout: Layout) => layout.items.find((item) => item.kind === "issue" && item.role === "current")!.box;

/** The hierarchy flavour of the shared canvas engine: groups/cards mode follows the scale. */
export function useCanvasView(layouts: Record<Mode, Layout>, viewportRef: RefObject<HTMLDivElement | null>) {
  const layoutsRef = useRef(layouts);
  layoutsRef.current = layouts;

  /** Groups and cards place the current card differently; keep it fixed on screen when the mode flips. */
  const adjust = useCallback((prev: View, target: View): View => {
    const [from, to] = [modeFor(prev.scale), modeFor(target.scale)];
    if (from === to) return target;
    const a = currentBox(layoutsRef.current[from]);
    const b = currentBox(layoutsRef.current[to]);
    return { ...target, x: target.x + (a.x - b.x) * target.scale, y: target.y + (a.y - b.y) * target.scale };
  }, []);

  const initialView = useCallback((size: Size) => fitAll(layoutsRef.current, size), []);
  const engine = useCanvasEngine({ viewportRef, initialView, adjust });
  const { jump } = engine;
  const me = useCallback(() => jump((size) => centerOn(currentBox(layoutsRef.current.cards), size, 1)), [jump]);

  const mode = modeFor(engine.view.scale);
  return { ...engine, mode, layout: layouts[mode], me, zoomIn: () => engine.zoomBy(ZOOM_STEP), zoomOut: () => engine.zoomBy(1 / ZOOM_STEP) };
}
