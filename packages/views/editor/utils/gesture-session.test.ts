// @vitest-environment node
import { describe, expect, it } from "vitest";
import {
  EMPTY_POINTER_SESSION,
  IDLE_SAFARI_GESTURE,
  classifyWheel,
  gestureChange,
  gestureEnd,
  gestureStart,
  pointerDown,
  pointerMove,
  pointerUp,
  shouldIgnoreWebkitWheel,
  type GestureEnv,
  type PointerSession,
  type WheelLike,
} from "./gesture-session";
import { MAX_SCALE, MIN_SCALE, type Point, type ZoomTransform } from "./zoom-transform";

const ENV: GestureEnv = {
  content: { width: 1000, height: 1000 },
  viewport: { width: 1000, height: 600 },
};
const START: ZoomTransform = { scale: 0.5, x: 100, y: 50 };

function down(session: PointerSession, id: number, p: Point, t: ZoomTransform) {
  return pointerDown(session, id, p, t);
}

function move(session: PointerSession, id: number, p: Point) {
  const out = pointerMove(session, id, p, ENV);
  return { session: out.session, transform: out.transform };
}

/** The content point drawn under a viewport point. */
function contentAt(t: ZoomTransform, p: Point): Point {
  return { x: (p.x - t.x) / t.scale, y: (p.y - t.y) / t.scale };
}

describe("pointer session — pinch", () => {
  it("2x spread doubles the scale and keeps the content point under the midpoint", () => {
    let s = down(EMPTY_POINTER_SESSION, 1, { x: 400, y: 300 }, START);
    s = down(s, 2, { x: 600, y: 300 }, START);
    const m0 = contentAt(START, { x: 500, y: 300 });
    s = move(s, 1, { x: 300, y: 300 }).session;
    const out = move(s, 2, { x: 700, y: 300 });
    expect(out.transform!.scale).toBeCloseTo(1, 10);
    const m1 = contentAt(out.transform!, { x: 500, y: 300 });
    expect(m1.x).toBeCloseTo(m0.x, 6);
    expect(m1.y).toBeCloseTo(m0.y, 6);
  });

  it("pans while pinching: both fingers moving 40px right moves x by 40px", () => {
    let s = down(EMPTY_POINTER_SESSION, 1, { x: 400, y: 300 }, START);
    s = down(s, 2, { x: 600, y: 300 }, START);
    s = move(s, 1, { x: 440, y: 300 }).session;
    const out = move(s, 2, { x: 640, y: 300 });
    expect(out.transform!.scale).toBeCloseTo(START.scale, 10);
    expect(out.transform!.x).toBeCloseTo(START.x + 40, 6);
    expect(out.transform!.y).toBeCloseTo(START.y, 6);
  });

  it("does not jump when a third finger lifts, and keeps tracking after", () => {
    let s = down(EMPTY_POINTER_SESSION, 1, { x: 400, y: 300 }, START);
    s = down(s, 2, { x: 600, y: 300 }, START);
    s = down(s, 3, { x: 500, y: 500 }, START);
    s = move(s, 1, { x: 410, y: 300 }).session;
    const before = move(s, 2, { x: 610, y: 300 }).transform!;
    s = pointerUp(s, 3, before);
    // Same positions, same transform: re-baselined, so the next move is a delta.
    const stay = move(s, 1, { x: 410, y: 300 }).transform!;
    expect(stay.x).toBeCloseTo(before.x, 6);
    expect(stay.y).toBeCloseTo(before.y, 6);
    expect(stay.scale).toBeCloseTo(before.scale, 10);
    const next = move(s, 1, { x: 420, y: 300 }).transform!;
    expect(next.x).toBeGreaterThan(before.x);
  });

  it("cancel of one finger mid-pinch leaves the other panning without a jump", () => {
    let s = down(EMPTY_POINTER_SESSION, 1, { x: 400, y: 300 }, START);
    s = down(s, 2, { x: 600, y: 300 }, START);
    s = move(s, 1, { x: 350, y: 300 }).session;
    const lastMove = move(s, 2, { x: 650, y: 300 });
    const pinched = lastMove.transform!;
    s = pointerUp(lastMove.session, 1, pinched);
    expect(s.pointers.map((p) => p.id)).toEqual([2]);
    const stay = move(s, 2, { x: 650, y: 300 }).transform!;
    expect(stay).toEqual(pinched);
    const panned = move(s, 2, { x: 680, y: 310 }).transform!;
    expect(panned.scale).toBeCloseTo(pinched.scale, 10);
    expect(panned.x).toBeCloseTo(pinched.x + 30, 6);
    expect(panned.y).toBeCloseTo(pinched.y + 10, 6);
  });

  it("at max scale both fingers moving still pans, anchor stable, no drift", () => {
    const atMax: ZoomTransform = { scale: MAX_SCALE, x: -1000, y: -1000 };
    let s = down(EMPTY_POINTER_SESSION, 1, { x: 400, y: 300 }, atMax);
    s = down(s, 2, { x: 600, y: 300 }, atMax);
    // Spread further: clamped scale stays, midpoint still follows.
    s = move(s, 1, { x: 350, y: 300 }).session;
    const a = move(s, 2, { x: 670, y: 300 }).transform!;
    expect(a.scale).toBe(MAX_SCALE);
    expect(a.x).toBeCloseTo(atMax.x + 10, 6); // midpoint 500 -> 510
    const b = move(s, 2, { x: 670, y: 300 }).transform!;
    expect(b).toEqual(a);
  });

  it("at min scale both fingers moving still pans", () => {
    const atMin: ZoomTransform = { scale: MIN_SCALE, x: 300, y: 100 };
    let s = down(EMPTY_POINTER_SESSION, 1, { x: 400, y: 300 }, atMin);
    s = down(s, 2, { x: 600, y: 300 }, atMin);
    // Pinch in (distance 200 -> 160) while the midpoint moves 20px right.
    s = move(s, 1, { x: 440, y: 300 }).session;
    const out = move(s, 2, { x: 600, y: 300 }).transform!;
    expect(out.scale).toBe(MIN_SCALE);
    expect(out.x).toBeCloseTo(atMin.x + 20, 6);
  });
});

describe("pointer session — single pointer", () => {
  it("pans 1:1 from the baseline", () => {
    let s = down(EMPTY_POINTER_SESSION, 1, { x: 100, y: 100 }, START);
    const out = move(s, 1, { x: 130, y: 90 });
    s = out.session;
    expect(out.transform).toEqual({ scale: 0.5, x: 130, y: 40 });
  });

  it("ignores moves of unknown pointers", () => {
    const s = down(EMPTY_POINTER_SESSION, 1, { x: 100, y: 100 }, START);
    expect(move(s, 9, { x: 0, y: 0 }).transform).toBeNull();
  });

  it("empties on the last pointer up", () => {
    let s = down(EMPTY_POINTER_SESSION, 1, { x: 100, y: 100 }, START);
    s = pointerUp(s, 1, START);
    expect(s).toEqual(EMPTY_POINTER_SESSION);
  });
});

function wheel(partial: Partial<WheelLike>): WheelLike {
  return {
    deltaX: 0,
    deltaY: 0,
    deltaMode: 0,
    ctrlKey: false,
    metaKey: false,
    shiftKey: false,
    ...partial,
  };
}

describe("classifyWheel", () => {
  it("zooms in on ctrlKey with negative deltaY (trackpad pinch)", () => {
    const a = classifyWheel(wheel({ ctrlKey: true, deltaY: -10 }));
    expect(a.type).toBe("zoom");
    if (a.type === "zoom") expect(a.factor).toBeGreaterThan(1);
  });

  it("zooms the same way for metaKey", () => {
    const ctrl = classifyWheel(wheel({ ctrlKey: true, deltaY: -10 }));
    expect(classifyWheel(wheel({ metaKey: true, deltaY: -10 }))).toEqual(ctrl);
  });

  it("is symmetric: opposite deltas return to the original scale", () => {
    const up = classifyWheel(wheel({ ctrlKey: true, deltaY: -10 }));
    const down = classifyWheel(wheel({ ctrlKey: true, deltaY: 10 }));
    if (up.type !== "zoom" || down.type !== "zoom") throw new Error("expected zoom");
    expect(up.factor * down.factor).toBeCloseTo(1, 10);
  });

  it("clamps each event to 18px, so a mouse notch is about one zoom step", () => {
    const notch = classifyWheel(wheel({ ctrlKey: true, deltaY: -100 }));
    const huge = classifyWheel(wheel({ ctrlKey: true, deltaY: -5000 }));
    if (notch.type !== "zoom" || huge.type !== "zoom") throw new Error("expected zoom");
    expect(notch.factor).toBeCloseTo(Math.exp(0.18), 10);
    expect(huge.factor).toBeCloseTo(notch.factor, 10);
  });

  it("does not clamp small trackpad pinch deltas", () => {
    const a = classifyWheel(wheel({ ctrlKey: true, deltaY: -5 }));
    if (a.type !== "zoom") throw new Error("expected zoom");
    expect(a.factor).toBeCloseTo(Math.exp(0.05), 10);
  });

  it("pans by the negated deltas without a modifier", () => {
    expect(classifyWheel(wheel({ deltaX: 30, deltaY: 12 }))).toEqual({
      type: "pan",
      dx: -30,
      dy: -12,
    });
  });

  it("normalizes line and page delta modes (16px / 100px)", () => {
    expect(classifyWheel(wheel({ deltaY: 2, deltaMode: 1 }))).toEqual({
      type: "pan",
      dx: -0,
      dy: -32,
    });
    expect(classifyWheel(wheel({ deltaY: 1, deltaMode: 2 }))).toEqual({
      type: "pan",
      dx: -0,
      dy: -100,
    });
    const line = classifyWheel(wheel({ ctrlKey: true, deltaY: -1, deltaMode: 1 }));
    const px = classifyWheel(wheel({ ctrlKey: true, deltaY: -16, deltaMode: 0 }));
    expect(line).toEqual(px);
  });

  it("shift + vertical wheel pans horizontally", () => {
    expect(classifyWheel(wheel({ shiftKey: true, deltaY: 40 }))).toEqual({
      type: "pan",
      dx: -40,
      dy: 0,
    });
  });

  it("shift keeps a real horizontal delta as-is", () => {
    expect(classifyWheel(wheel({ shiftKey: true, deltaX: 20, deltaY: 40 }))).toEqual({
      type: "pan",
      dx: -20,
      dy: -40,
    });
  });
});

describe("Safari gesture adapter", () => {
  const anchor = { x: 500, y: 300 };
  const base: ZoomTransform = { scale: 1, x: 0, y: 0 };

  it("gesturechange scale 1.5 zooms 1.5x at the anchor", () => {
    const s = gestureStart(IDLE_SAFARI_GESTURE, base, anchor);
    expect(s.active).toBe(true);
    const t = gestureChange(s, 1.5, false, ENV)!;
    expect(t.scale).toBeCloseTo(1.5, 10);
    const before = contentAt(base, anchor);
    const after = contentAt(t, anchor);
    expect(after.x).toBeCloseTo(before.x, 6);
    expect(after.y).toBeCloseTo(before.y, 6);
  });

  it("scales from the gesturestart baseline, not cumulatively", () => {
    const s = gestureStart(IDLE_SAFARI_GESTURE, base, anchor);
    gestureChange(s, 1.5, false, ENV);
    expect(gestureChange(s, 1.2, false, ENV)!.scale).toBeCloseTo(1.2, 10);
  });

  it("ignores gesture scale while a touch pointer is active", () => {
    const s = gestureStart(IDLE_SAFARI_GESTURE, base, anchor);
    expect(gestureChange(s, 1.5, true, ENV)).toBeNull();
  });

  it("ignores gesturechange without a gesturestart", () => {
    expect(gestureChange(IDLE_SAFARI_GESTURE, 1.5, false, ENV)).toBeNull();
  });

  it("ignores a ctrlKey wheel during an active gesture", () => {
    const s = gestureStart(IDLE_SAFARI_GESTURE, base, anchor);
    expect(shouldIgnoreWebkitWheel(s, { deltaY: -100, ctrlKey: true, metaKey: false }, 0)).toBe(true);
  });

  it("ignores a ctrlKey wheel within 100ms after gestureend, not after", () => {
    const s = gestureEnd(gestureStart(IDLE_SAFARI_GESTURE, base, anchor), 1000);
    expect(s.active).toBe(false);
    expect(shouldIgnoreWebkitWheel(s, { deltaY: -100, ctrlKey: true, metaKey: false }, 1050)).toBe(true);
    expect(shouldIgnoreWebkitWheel(s, { deltaY: -100, ctrlKey: true, metaKey: false }, 1101)).toBe(false);
  });

  it("ignores a fractional-deltaY ctrlKey wheel (trackpad) before gesturestart", () => {
    expect(
      shouldIgnoreWebkitWheel(IDLE_SAFARI_GESTURE, { deltaY: -3.25, ctrlKey: true, metaKey: false }, 0),
    ).toBe(true);
  });

  it("lets an integer ctrlKey mouse wheel zoom when no gesture is running", () => {
    expect(
      shouldIgnoreWebkitWheel(IDLE_SAFARI_GESTURE, { deltaY: 100, ctrlKey: true, metaKey: false }, 0),
    ).toBe(false);
  });

  it("metaKey wheel always zooms, even mid-gesture or fractional", () => {
    const s = gestureStart(IDLE_SAFARI_GESTURE, base, anchor);
    expect(shouldIgnoreWebkitWheel(s, { deltaY: -3.25, ctrlKey: false, metaKey: true }, 0)).toBe(false);
    expect(shouldIgnoreWebkitWheel(s, { deltaY: -3.25, ctrlKey: true, metaKey: true }, 0)).toBe(false);
  });

  it("never ignores a plain wheel", () => {
    expect(
      shouldIgnoreWebkitWheel(IDLE_SAFARI_GESTURE, { deltaY: 3.5, ctrlKey: false, metaKey: false }, 0),
    ).toBe(false);
  });
});
