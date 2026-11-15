import { afterEach, describe, expect, it, vi } from "vitest";
import { renderHook } from "@testing-library/react";
import { useScrollPointerShield } from "./use-scroll-pointer-shield";

afterEach(() => vi.useRealTimers());

describe("useScrollPointerShield", () => {
  it("covers the list while scrolling and removes the cover 150 ms after the last scroll", () => {
    vi.useFakeTimers();
    const scroller = document.createElement("div");
    const shield = () => scroller.querySelector("[data-scroll-shield]");
    renderHook(() => useScrollPointerShield(scroller));

    scroller.dispatchEvent(new Event("scroll"));
    expect(shield()).not.toBeNull();
    vi.advanceTimersByTime(100);
    scroller.dispatchEvent(new Event("scroll"));
    vi.advanceTimersByTime(100);
    expect(shield()).not.toBeNull();
    vi.advanceTimersByTime(60);
    expect(shield()).toBeNull();
  });
});
