import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { mentionsViewer, NextSelfMention } from "./next-self-mention";

vi.mock("../i18n", async () => {
  const editor = (await import("../locales/en/editor.json")).default;
  return {
    useT: () => ({
      t: (select: (bundle: typeof editor) => string) => select(editor),
    }),
  };
});

const VIEWER = "f0c4c4cf-b6cd-4939-bfba-a5e14290c1ff";
const OTHER = "8f14e45f-ceea-4d0e-a1a2-9b1c0d3e4f5a";

describe("mentionsViewer", () => {
  it("matches a member mention of the viewer and @all", () => {
    expect(mentionsViewer(`hi [@Me](mention://member/${VIEWER})`, VIEWER)).toBe(true);
    expect(mentionsViewer("hi [@all](mention://all/all)", VIEWER)).toBe(true);
  });

  it("ignores other members, agents, no viewer and code", () => {
    expect(mentionsViewer(`[@O](mention://member/${OTHER})`, VIEWER)).toBe(false);
    expect(mentionsViewer(`[@A](mention://agent/${VIEWER})`, VIEWER)).toBe(false);
    expect(mentionsViewer("[@all](mention://all/all)", undefined)).toBe(false);
    expect(mentionsViewer("`[@all](mention://all/all)`", VIEWER)).toBe(false);
    expect(mentionsViewer("```\n[@all](mention://all/all)\n```", VIEWER)).toBe(false);
    expect(mentionsViewer(null, VIEWER)).toBe(false);
  });

  it("ignores mentions in tilde fences and indented code", () => {
    expect(mentionsViewer("~~~\n[@all](mention://all/all)\n~~~", VIEWER)).toBe(false);
    expect(mentionsViewer("text\n\n    [@all](mention://all/all)\n", VIEWER)).toBe(false);
  });

  it("matches legacy member shortcodes the renderer normalizes", () => {
    expect(mentionsViewer(`hi [@ id="${VIEWER}" label="Me"]`, VIEWER)).toBe(true);
    expect(mentionsViewer(`hi [@ id="${OTHER}" label="Them"]`, VIEWER)).toBe(false);
  });
});

// jsdom has no layout: the scroller is 0..100px tall and each mounted row sits
// at the top the test gives it.
function mountList(rows: Record<number, number>) {
  const scroller = document.createElement("div");
  scroller.getBoundingClientRect = () => ({ top: 0, bottom: 100 }) as DOMRect;
  for (const [index, top] of Object.entries(rows)) {
    const row = document.createElement("div");
    row.setAttribute("data-item-index", index);
    row.getBoundingClientRect = () => ({ top, bottom: top + 40 }) as DOMRect;
    scroller.append(row);
  }
  document.body.append(scroller);
  return scroller;
}

describe("NextSelfMention", () => {
  beforeEach(() => {
    // Frames run inline; each one costs 8 ms on the clock the settle budget reads.
    let clock = 0;
    vi.spyOn(performance, "now").mockImplementation(() => clock);
    vi.stubGlobal("requestAnimationFrame", (cb: FrameRequestCallback) => {
      clock += 8;
      cb(clock);
      return 0;
    });
  });
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    document.body.innerHTML = "";
  });

  it("renders nothing when no self-mention is below the viewport", () => {
    // Rows 0-1 are on screen; the only mention is in visible row 1.
    const scroller = mountList({ 0: 0, 1: 50 });
    render(<NextSelfMention scrollEl={scroller} flags={[false, true, false]} onJump={vi.fn()} />);
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("shows the button when every mounted row is below the viewport", () => {
    // A long description above the timeline: row 0 starts past the scroller's bottom edge.
    const scroller = mountList({ 0: 150, 1: 200 });
    const onJump = vi.fn();
    render(<NextSelfMention scrollEl={scroller} flags={[true, false]} onJump={onJump} />);
    fireEvent.click(screen.getByRole("button"));
    expect(onJump).toHaveBeenCalledWith(0);
  });

  it("hides the button when every mounted row is above the viewport and holds no mention", () => {
    const scroller = mountList({ 0: -200, 1: -150 });
    render(<NextSelfMention scrollEl={scroller} flags={[true, false]} onJump={vi.fn()} />);
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("jumps to the first self-mention below the viewport", () => {
    const scroller = mountList({ 0: 0, 1: 50 });
    const onJump = vi.fn();
    render(<NextSelfMention scrollEl={scroller} flags={[false, true, false, true]} onJump={onJump} />);
    const button = screen.getByRole("button", { name: /next mention of me/i });
    expect(screen.queryByTestId("next-self-mention-count")).toBeNull();
    fireEvent.click(button);
    expect(onJump).toHaveBeenCalledWith(3);
  });

  it("shows the count when more than one is below, and honours firstItemIndex", () => {
    // Virtuoso numbers rows from firstItemIndex (here 100): rows 100-101 visible.
    const scroller = mountList({ 100: 0, 101: 50 });
    const onJump = vi.fn();
    render(
      <NextSelfMention
        scrollEl={scroller}
        flags={[false, false, true, true, true]}
        firstItemIndex={100}
        onJump={onJump}
      />,
    );
    expect(screen.getByTestId("next-self-mention-count")).toHaveTextContent("3");
    fireEvent.click(screen.getByRole("button"));
    expect(onJump).toHaveBeenCalledWith(2);
  });

  it("recomputes when the reader scrolls", () => {
    const scroller = mountList({ 0: 0, 1: 50 });
    render(<NextSelfMention scrollEl={scroller} flags={[false, false, true]} onJump={vi.fn()} />);
    expect(screen.getByRole("button")).toBeInTheDocument();
    // Scroll: row 2 now on screen, rows 0-1 unmounted.
    scroller.replaceChildren();
    const row = document.createElement("div");
    row.setAttribute("data-item-index", "2");
    row.getBoundingClientRect = () => ({ top: 10, bottom: 50 }) as DOMRect;
    scroller.append(row);
    act(() => {
      scroller.dispatchEvent(new Event("scroll"));
    });
    expect(screen.queryByRole("button")).toBeNull();
  });

  // A jump mounts row 3; `tops` is where it sits after each successive jump.
  function jumpMounting(scroller: HTMLElement, tops: number[]) {
    let calls = 0;
    return vi.fn(() => {
      let row = scroller.querySelector<HTMLElement>('[data-item-index="3"]');
      if (!row) {
        row = document.createElement("div");
        row.setAttribute("data-item-index", "3");
        scroller.append(row);
      }
      const top = tops[Math.min(calls++, tops.length - 1)]!;
      row.getBoundingClientRect = () => ({ top, bottom: top + 40 }) as DOMRect;
    });
  }

  it("re-issues the jump until the target row has settled at the top", () => {
    // Estimated heights: the first two jumps land the row off the top.
    const scroller = mountList({ 0: 0, 1: 50 });
    const onJump = jumpMounting(scroller, [70, 55, 4]);
    render(<NextSelfMention scrollEl={scroller} flags={[false, false, false, true]} onJump={onJump} />);
    fireEvent.click(screen.getByRole("button"));
    expect(onJump).toHaveBeenCalledTimes(3);
    expect(onJump).toHaveBeenCalledWith(3);
  });

  it("jumps once when the target row lands at the top", () => {
    const scroller = mountList({ 0: 0, 1: 50 });
    const onJump = jumpMounting(scroller, [4]);
    render(<NextSelfMention scrollEl={scroller} flags={[false, false, false, true]} onJump={onJump} />);
    fireEvent.click(screen.getByRole("button"));
    expect(onJump).toHaveBeenCalledTimes(1);
  });
  describe("with frames the test paces", () => {
    let queue: FrameRequestCallback[];
    let now: number;
    beforeEach(() => {
      queue = [];
      now = 0;
      vi.spyOn(performance, "now").mockImplementation(() => now);
      vi.stubGlobal("requestAnimationFrame", (cb: FrameRequestCallback) => queue.push(cb));
    });
    // One display frame: run what was queued before it, then advance the clock.
    const frame = (ms: number) => {
      const due = queue.splice(0);
      now += ms;
      act(() => due.forEach((cb) => cb(now)));
    };

    it("keeps settling when the host re-renders with new flags mid-jump", () => {
      const scroller = mountList({ 0: 0, 1: 50 });
      const onJump = jumpMounting(scroller, [70, 55, 4]);
      const { rerender } = render(<NextSelfMention scrollEl={scroller} flags={[false, false, false, true]} onJump={onJump} />);
      frame(8);
      fireEvent.click(screen.getByRole("button"));
      rerender(<NextSelfMention scrollEl={scroller} flags={[false, false, false, true]} onJump={onJump} />);
      for (let i = 0; i < 10; i++) frame(8);
      expect(onJump).toHaveBeenCalledTimes(3);
    });

    it("budgets the settle in time, not frames, so a 120 Hz display gets the same second", () => {
      const scroller = mountList({ 0: 0, 1: 50 });
      // Row 3 lands off the top for 100 frames (~0.8 s at 8 ms), then settles.
      const tops = [...Array<number>(100).fill(70), 4];
      const onJump = jumpMounting(scroller, tops);
      render(<NextSelfMention scrollEl={scroller} flags={[false, false, false, true]} onJump={onJump} />);
      frame(8);
      fireEvent.click(screen.getByRole("button"));
      for (let i = 0; i < 130; i++) frame(8);
      expect(onJump).toHaveBeenCalledTimes(101);
    });

    it("gives up after about a second", () => {
      const scroller = mountList({ 0: 0, 1: 50 });
      const onJump = jumpMounting(scroller, [70]);
      render(<NextSelfMention scrollEl={scroller} flags={[false, false, false, true]} onJump={onJump} />);
      frame(8);
      fireEvent.click(screen.getByRole("button"));
      for (let i = 0; i < 400; i++) frame(8);
      expect(onJump.mock.calls.length).toBeLessThan(140);
    });
  });
});
