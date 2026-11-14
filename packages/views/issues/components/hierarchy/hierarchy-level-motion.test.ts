// @vitest-environment node
import { describe, expect, it } from "vitest";
import { levelMotion } from "./hierarchy-level-motion";

describe("levelMotion", () => {
  it("default: 240 ms ease-out slide, as locked in the design", () => {
    expect(levelMotion(false)).toEqual({ durationMs: 240, easing: "ease-out", slide: true, transition: "transform 240ms ease-out, opacity 240ms ease-out" });
  });

  it("prefers-reduced-motion: instant, no slide, no transition", () => {
    expect(levelMotion(true)).toEqual({ durationMs: 0, easing: "linear", slide: false, transition: "none" });
  });
});
