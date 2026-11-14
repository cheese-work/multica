import { afterEach, describe, expect, it, vi } from "vitest";
import { renderHook } from "@testing-library/react";
import { useScrollPointerShield } from "./use-scroll-pointer-shield";

afterEach(() => vi.useRealTimers());

describe("useScrollPointerShield", () => {
  it("ignores the pointer while scrolling and restores it 150 ms after the last scroll", () => {
    vi.useFakeTimers();
    const scroller = document.createElement("div");
    const target = document.createElement("div");
    renderHook(() => useScrollPointerShield({ current: target }, scroller));

    scroller.dispatchEvent(new Event("scroll"));
    expect(target.style.pointerEvents).toBe("none");
    vi.advanceTimersByTime(100);
    scroller.dispatchEvent(new Event("scroll"));
    vi.advanceTimersByTime(100);
    expect(target.style.pointerEvents).toBe("none");
    vi.advanceTimersByTime(60);
    expect(target.style.pointerEvents).toBe("");
  });
});
