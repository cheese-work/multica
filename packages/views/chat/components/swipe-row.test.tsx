import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { SwipeRow, type SwipeAction } from "./swipe-row";

const act1: SwipeAction = { key: "pin", label: "Pin", icon: <i />, onSelect: vi.fn() };
const act2: SwipeAction = { key: "archive", label: "Archive", icon: <i />, onSelect: vi.fn() };
const act3: SwipeAction = { key: "more", label: "More", icon: <i />, onSelect: vi.fn() };

function setup(
  props: Partial<React.ComponentProps<typeof SwipeRow>> = {},
) {
  const onFullSwipeLeft = vi.fn();
  const onLongPress = vi.fn();
  const onClick = vi.fn();
  render(
    <SwipeRow
      leftActions={[act1, act2, act3]}
      rightAction={{ key: "seen", label: "Mark as seen", icon: <i />, onSelect: vi.fn() }}
      onFullSwipeLeft={onFullSwipeLeft}
      onLongPress={onLongPress}
      {...props}
    >
      <div data-testid="fg" onClick={onClick}>
        Row
      </div>
    </SwipeRow>,
  );
  const fg = screen.getByTestId("fg");
  return { fg, onFullSwipeLeft, onLongPress, onClick };
}

const touch = { pointerId: 1, pointerType: "touch", isPrimary: true };

function drag(el: Element, dx: number, dy = 0) {
  fireEvent.pointerDown(el, { ...touch, clientX: 300, clientY: 20 });
  // A few moves, like a real finger.
  fireEvent.pointerMove(el, { ...touch, clientX: 300 + dx / 2, clientY: 20 + dy / 2 });
  fireEvent.pointerMove(el, { ...touch, clientX: 300 + dx, clientY: 20 + dy });
  fireEvent.pointerUp(el, { ...touch, clientX: 300 + dx, clientY: 20 + dy });
}

beforeEach(() => {
  vi.useFakeTimers();
  for (const a of [act1, act2, act3]) (a.onSelect as ReturnType<typeof vi.fn>).mockClear();
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("SwipeRow reveal", () => {
  it("reveals the left actions in order after a left drag past the threshold", () => {
    const { fg } = setup();
    expect(screen.queryByRole("button", { name: "Pin" })).not.toBeInTheDocument();
    drag(fg, -120);
    const names = screen.getAllByRole("button").map((b) => b.getAttribute("aria-label"));
    expect(names).toEqual(["Pin", "Archive", "More"]);
  });

  it("reveals the right action after a right drag, not the left ones", () => {
    const { fg } = setup();
    drag(fg, 120);
    expect(screen.getByRole("button", { name: "Mark as seen" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Pin" })).not.toBeInTheDocument();
  });

  it("stays closed below the threshold", () => {
    const { fg } = setup();
    drag(fg, -20);
    expect(screen.queryByRole("button", { name: "Pin" })).not.toBeInTheDocument();
  });

  it("ignores a mostly vertical drag (list scroll)", () => {
    const { fg } = setup();
    drag(fg, -80, 200);
    expect(screen.queryByRole("button", { name: "Pin" })).not.toBeInTheDocument();
  });

  it("ignores mouse pointers", () => {
    const { fg } = setup();
    const mouse = { pointerId: 2, pointerType: "mouse", isPrimary: true };
    fireEvent.pointerDown(fg, { ...mouse, clientX: 300, clientY: 20 });
    fireEvent.pointerMove(fg, { ...mouse, clientX: 100, clientY: 20 });
    fireEvent.pointerUp(fg, { ...mouse, clientX: 100, clientY: 20 });
    expect(screen.queryByRole("button", { name: "Pin" })).not.toBeInTheDocument();
  });

  it("runs the tapped action once and closes the tray", () => {
    const { fg } = setup();
    drag(fg, -120);
    fireEvent.click(screen.getByRole("button", { name: "Archive" }));
    expect(act2.onSelect).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("button", { name: "Pin" })).not.toBeInTheDocument();
  });

  it("closes on a tap of the row and does not forward that click", () => {
    const { fg, onClick } = setup();
    drag(fg, -120);
    act(() => void vi.advanceTimersByTime(150));
    fireEvent.click(fg);
    expect(onClick).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "Pin" })).not.toBeInTheDocument();
  });

  it("does not forward the click that ends a drag, nor close the tray on it", () => {
    const { fg, onClick } = setup();
    drag(fg, -120);
    fireEvent.click(fg); // the drag's own click tail
    expect(onClick).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Pin" })).toBeInTheDocument();
    act(() => void vi.advanceTimersByTime(150));
    fireEvent.click(fg); // a real tap closes
    expect(screen.queryByRole("button", { name: "Pin" })).not.toBeInTheDocument();
    fireEvent.click(fg); // closed: a normal tap passes through
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it("gives every revealed action a 44x44 minimum hit area", () => {
    const { fg } = setup();
    drag(fg, -120);
    for (const b of screen.getAllByRole("button")) {
      expect(b.className).toMatch(/min-h-11/);
      expect(b.className).toMatch(/min-w-11/);
    }
  });
});

describe("SwipeRow full swipe", () => {
  it("fires onFullSwipeLeft once on a full left swipe and does not reveal the tray", () => {
    const { fg, onFullSwipeLeft } = setup();
    drag(fg, -320);
    expect(onFullSwipeLeft).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("button", { name: "Pin" })).not.toBeInTheDocument();
  });

  it("only reveals when no full-swipe handler is given", () => {
    const { fg } = setup({ onFullSwipeLeft: undefined });
    drag(fg, -320);
    expect(screen.getByRole("button", { name: "Pin" })).toBeInTheDocument();
  });
});

describe("SwipeRow long press", () => {
  it("fires onLongPress once after 500 ms without movement", () => {
    const { fg, onLongPress, onClick } = setup();
    fireEvent.pointerDown(fg, { ...touch, clientX: 300, clientY: 20 });
    act(() => void vi.advanceTimersByTime(499));
    expect(onLongPress).not.toHaveBeenCalled();
    act(() => void vi.advanceTimersByTime(1));
    expect(onLongPress).toHaveBeenCalledTimes(1);
    fireEvent.pointerUp(fg, { ...touch, clientX: 300, clientY: 20 });
    // The click synthesized after the press must not select the row.
    fireEvent.click(fg);
    expect(onClick).not.toHaveBeenCalled();
    act(() => void vi.advanceTimersByTime(2000));
    expect(onLongPress).toHaveBeenCalledTimes(1);
  });

  it("is cancelled by movement", () => {
    const { fg, onLongPress } = setup();
    fireEvent.pointerDown(fg, { ...touch, clientX: 300, clientY: 20 });
    fireEvent.pointerMove(fg, { ...touch, clientX: 280, clientY: 20 });
    act(() => void vi.advanceTimersByTime(1000));
    expect(onLongPress).not.toHaveBeenCalled();
  });

  it("is cancelled by an early release", () => {
    const { fg, onLongPress } = setup();
    fireEvent.pointerDown(fg, { ...touch, clientX: 300, clientY: 20 });
    act(() => void vi.advanceTimersByTime(200));
    fireEvent.pointerUp(fg, { ...touch, clientX: 300, clientY: 20 });
    act(() => void vi.advanceTimersByTime(1000));
    expect(onLongPress).not.toHaveBeenCalled();
  });
});

describe("SwipeRow reduced motion", () => {
  it("drops the settle transition under prefers-reduced-motion", () => {
    const { fg } = setup();
    expect(fg.parentElement!.className).toMatch(/motion-reduce:transition-none/);
  });
});
