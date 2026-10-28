import { act, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { createRowRangeSource } from "../issues/components/thread-minimap";
import { contentMentionsViewer, useNextSelfMention } from "./next-self-mention";
import { NextSelfMentionButton } from "./next-self-mention-button";

vi.mock("../i18n", async () => {
  const common = (await import("../locales/en/common.json")).default;
  return {
    useT: () => ({
      t: (select: (bundle: typeof common) => string, vars?: Record<string, unknown>) => {
        const out = select(common);
        return out.replace(/\{\{(\w+)\}\}/g, (_, k: string) => String(vars?.[k] ?? ""));
      },
    }),
  };
});

const VIEWER = "f0c4c4cf-b6cd-4939-bfba-a5e14290c1ff";
const OTHER = "8f14e45f-ceea-4d0e-a1a2-9b1c0d3e4f5a";

describe("contentMentionsViewer", () => {
  it("matches a member mention of the viewer", () => {
    expect(contentMentionsViewer(`hi [@Me](mention://member/${VIEWER})`, VIEWER)).toBe(true);
  });

  it("matches @all for any signed-in viewer", () => {
    expect(contentMentionsViewer("[@all](mention://all/all)", VIEWER)).toBe(true);
  });

  it("ignores other members, agents with the viewer's id, and plain text", () => {
    expect(contentMentionsViewer(`[@O](mention://member/${OTHER})`, VIEWER)).toBe(false);
    expect(contentMentionsViewer(`[@A](mention://agent/${VIEWER})`, VIEWER)).toBe(false);
    expect(contentMentionsViewer("@all hello", VIEWER)).toBe(false);
  });

  it("ignores mentions shown as code", () => {
    expect(contentMentionsViewer(`\`[@Me](mention://member/${VIEWER})\``, VIEWER)).toBe(false);
    expect(
      contentMentionsViewer(`\`\`\`\n[@Me](mention://member/${VIEWER})\n\`\`\``, VIEWER),
    ).toBe(false);
  });

  it("is false without a viewer or content", () => {
    expect(contentMentionsViewer("[@all](mention://all/all)", undefined)).toBe(false);
    expect(contentMentionsViewer(undefined, VIEWER)).toBe(false);
  });
});

// jsdom has no layout: rects are stubbed from data-top, the container is 600px tall.
const VIEWPORT = 600;
function rect(top: number, height: number) {
  return { top, bottom: top + height, height, left: 0, right: 0, width: 0, x: 0, y: top, toJSON: () => ({}) } as DOMRect;
}

// Live positions, so a test can scroll without a re-render restubbing the nodes.
let tops: number[] = [];

interface HarnessProps {
  flags: boolean[];
  mentionTops: number[];
  indexOffset?: number;
  scrollToIndex?: (index: number) => void;
  range?: ReturnType<typeof createRowRangeSource>;
}

function Harness({ flags, mentionTops, indexOffset = 0, scrollToIndex = () => {}, range }: HarnessProps) {
  tops = mentionTops;
  const rangeSource = range ?? createRowRangeSource();
  const [el, setEl] = useState<HTMLDivElement | null>(null);
  const { count, jump } = useNextSelfMention({
    container: el,
    flags,
    range: rangeSource,
    indexOffset,
    scrollToIndex,
  });
  return (
    <div>
      <div
        data-testid="scroller"
        ref={(node) => {
          if (node) {
            node.getBoundingClientRect = () => rect(0, VIEWPORT);
            node.scrollTo = vi.fn();
          }
          setEl(node);
        }}
      >
        {mentionTops.map((_, i) => (
          <span
            key={i}
            className="mention-self"
            data-testid={`mention-${i}`}
            ref={(node) => {
              if (node) node.getBoundingClientRect = () => rect(tops[i]!, 20);
            }}
          />
        ))}
      </div>
      <NextSelfMentionButton count={count} onJump={jump} />
    </div>
  );
}

describe("next self-mention button", () => {
  beforeEach(() => {
    vi.stubGlobal("requestAnimationFrame", (cb: FrameRequestCallback) => {
      cb(0);
      return 0;
    });
  });

  it("is absent when no self-mention is in the list", () => {
    render(<Harness flags={[false, false]} mentionTops={[]} />);
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("is absent when every self-mention is already on screen", () => {
    render(<Harness flags={[true]} mentionTops={[100]} />);
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("shows a button without a count for one mention below the viewport", () => {
    render(<Harness flags={[true]} mentionTops={[900]} />);
    const button = screen.getByRole("button", { name: /next mention/i });
    expect(button.textContent).not.toMatch(/\d/);
  });

  it("scrolls to the next mention below the viewport", () => {
    render(<Harness flags={[true, true]} mentionTops={[100, 900, 1500]} />);
    const scroller = screen.getByTestId("scroller");
    fireEvent.click(screen.getByRole("button", { name: /next mention/i }));
    // 900 - container top 0 - a third of the viewport (200) = 700 from the current scrollTop (0)
    expect(scroller.scrollTo).toHaveBeenCalledWith(expect.objectContaining({ top: 700 }));
  });

  it("shows the count when more than one mention is below the viewport", () => {
    render(<Harness flags={[true, true]} mentionTops={[900, 1500]} />);
    expect(screen.getByRole("button", { name: /next mention/i }).textContent).toContain("2");
  });

  it("counts and reaches rows Virtuoso has not mounted yet", () => {
    const range = createRowRangeSource();
    range.set({ start: 0, end: 2 });
    const scrollToIndex = vi.fn();
    render(
      <Harness
        flags={[false, false, false, false, true, true]}
        mentionTops={[]}
        range={range}
        scrollToIndex={scrollToIndex}
      />,
    );
    const button = screen.getByRole("button", { name: /next mention/i });
    expect(button.textContent).toContain("2");
    fireEvent.click(button);
    expect(scrollToIndex).toHaveBeenCalledWith(4);
  });

  it("maps Virtuoso's absolute indexes through indexOffset", () => {
    const range = createRowRangeSource();
    range.set({ start: 1000, end: 1002 });
    const scrollToIndex = vi.fn();
    render(
      <Harness
        flags={[false, false, false, true]}
        mentionTops={[]}
        range={range}
        indexOffset={1000}
        scrollToIndex={scrollToIndex}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /next mention/i }));
    expect(scrollToIndex).toHaveBeenCalledWith(3);
  });

  it("hides again after the reader scrolls past the last mention", () => {
    render(<Harness flags={[true]} mentionTops={[900]} />);
    expect(screen.getByRole("button")).toBeTruthy();
    tops[0] = 300;
    act(() => {
      fireEvent.scroll(screen.getByTestId("scroller"));
    });
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("is keyboard reachable: a native button that Enter activates", () => {
    render(<Harness flags={[true]} mentionTops={[900]} />);
    const button = screen.getByRole("button");
    expect(button.tagName).toBe("BUTTON");
    expect(button.getAttribute("tabindex")).not.toBe("-1");
  });
});
