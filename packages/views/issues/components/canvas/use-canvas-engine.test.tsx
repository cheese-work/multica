import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fitView, zoomAt } from "./canvas-math";
import { useCanvasEngine } from "./use-canvas-engine";

const viewport = (w = 800, h = 600) => {
  const el = document.createElement("div");
  el.getBoundingClientRect = () => ({ width: w, height: h, x: 0, y: 0, top: 0, left: 0, right: w, bottom: h, toJSON: () => ({}) });
  return { current: el };
};

const mockMotion = (reduce: boolean) =>
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: reduce && query.includes("prefers-reduced-motion"),
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
  }));

const mount = (ref = viewport()) =>
  renderHook(() => useCanvasEngine({ viewportRef: ref, initialView: () => ({ x: 0, y: 0, scale: 1 }) }));

beforeEach(() => mockMotion(false));
afterEach(() => vi.unstubAllGlobals());

describe("canvas math: unbounded zoom", () => {
  it("zoomAt has no fixed lower or upper bound", () => {
    const view = { x: 0, y: 0, scale: 1 };
    expect(zoomAt(view, 9, { x: 0, y: 0 }).scale).toBe(9);
    expect(zoomAt(view, 0.01, { x: 0, y: 0 }).scale).toBe(0.01);
  });
  it("zoomAt keeps the previous view for a degenerate scale", () => {
    const view = { x: 5, y: 6, scale: 1 };
    for (const bad of [0, -1, Number.NaN, Infinity]) expect(zoomAt(view, bad, { x: 0, y: 0 })).toEqual(view);
  });
  it("zoomAt keeps the previous view when the new offsets overflow", () => {
    const view = { x: 1e300, y: -1e300, scale: 1 };
    expect(zoomAt(view, 1e300, { x: 10, y: 10 })).toEqual(view);
  });
  it("fitView of huge bounds goes below the old 20% floor", () => {
    expect(fitView({ x: 0, y: 0, w: 100_000, h: 100_000 }, { w: 800, h: 600 }).scale).toBeLessThan(0.2);
  });
});

describe("useCanvasEngine", () => {
  it("zooms past the old 20%–200% bounds", () => {
    const { result } = mount();
    act(() => { for (let i = 0; i < 20; i++) result.current.zoomIn(); });
    expect(result.current.view.scale).toBeGreaterThan(2);
    act(() => { for (let i = 0; i < 60; i++) result.current.zoomOut(); });
    expect(result.current.view.scale).toBeLessThan(0.2);
  });
  it("keeps the view finite and positive after extreme zooming", () => {
    const { result } = mount();
    act(() => { for (let i = 0; i < 4000; i++) result.current.zoomBy(2, undefined, false); });
    expect(Number.isFinite(result.current.view.scale)).toBe(true);
    expect(result.current.view.scale).toBeGreaterThan(0);
  });
  it("keeps the view coordinates finite while zooming in repeatedly", () => {
    const { result } = mount();
    act(() => { for (let i = 0; i < 4000; i++) result.current.zoomBy(2, { x: 1, y: 1 }, false); });
    const { x, y, scale } = result.current.view;
    expect([x, y, scale].every(Number.isFinite)).toBe(true);
  });
  it("animates button zoom by default", () => {
    const { result } = mount();
    act(() => result.current.zoomIn());
    expect(result.current.smooth).toBe(true);
  });
  it("never animates under prefers-reduced-motion", () => {
    mockMotion(true);
    const { result } = mount();
    act(() => result.current.zoomIn());
    expect(result.current.view.scale).toBeCloseTo(1.25);
    expect(result.current.smooth).toBe(false);
    act(() => result.current.fit());
    expect(result.current.smooth).toBe(false);
  });
});
