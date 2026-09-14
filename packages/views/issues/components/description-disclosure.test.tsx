// @vitest-environment jsdom

import { useState } from "react";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const measurement = vi.hoisted(() => ({
  current: {
    totalRows: 13,
    hiddenRows: 1,
    hasOverflow: true,
    lineHeight: 20,
    previewText: "Visible description only",
  },
}));

vi.mock("./description-measurement", () => ({
  DESCRIPTION_PREVIEW_LINES: 12,
  measureDescription: () => measurement.current,
}));

import { DescriptionDisclosure } from "./description-disclosure";

const labels = {
  preview: "Description preview",
  loading: "Description preview is loading",
  showMore: "Show more",
  showLess: "Show less",
  moreLines: (count: number) => `${count} more line${count === 1 ? "" : "s"}`,
};

class TestResizeObserver {
  static callback: ResizeObserverCallback | null = null;
  constructor(callback: ResizeObserverCallback) {
    TestResizeObserver.callback = callback;
  }
  observe() {}
  disconnect() {}
}

function DisclosureHarness() {
  const [expanded, setExpanded] = useState(false);
  return (
    <DescriptionDisclosure
      contentVersion="test"
      expanded={expanded}
      id="issue-1"
      labels={labels}
      onExpandedChange={setExpanded}
    >
      <div data-testid="real-editor">
        Visible description only <a href="#hidden">Hidden suffix</a>
      </div>
    </DescriptionDisclosure>
  );
}

beforeEach(() => {
  measurement.current = {
    totalRows: 13,
    hiddenRows: 1,
    hasOverflow: true,
    lineHeight: 20,
    previewText: "Visible description only",
  };
  vi.stubGlobal("ResizeObserver", TestResizeObserver);
  vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => {
    callback(0);
    return 1;
  });
  vi.stubGlobal("cancelAnimationFrame", () => undefined);
});

describe("DescriptionDisclosure", () => {
  it("keeps one child mounted while collapsed and expanded", () => {
    render(<DisclosureHarness />);

    const editor = screen.getByTestId("real-editor").parentElement!;
    expect(editor).toHaveAttribute("aria-hidden", "true");
    expect(editor).toHaveAttribute("inert");
    expect(screen.getByText("Visible description only", { selector: "[data-description-accessible-preview] span" })).toBeInTheDocument();
    expect(screen.queryByText("Hidden suffix", { selector: "[data-description-accessible-preview]" })).not.toBeInTheDocument();
    const showMore = screen.getByRole("button", { name: "Show more 1 more line" });
    expect(showMore).toHaveAttribute("aria-expanded", "false");
    expect(showMore).toHaveAttribute("aria-controls", editor.id);

    const child = screen.getByTestId("real-editor");
    fireEvent.click(screen.getByRole("button", { name: "Show more 1 more line" }));

    expect(screen.getByTestId("real-editor")).toBe(child);
    expect(editor).not.toHaveAttribute("aria-hidden");
    expect(editor).not.toHaveAttribute("inert");
    expect(document.querySelector("[data-description-accessible-preview]")).toBeNull();
    expect(screen.getByRole("button", { name: "Show less" })).toHaveAttribute("aria-expanded", "true");
  });

  it("expands from a pointerdown on the non-inert section, not the inert editor div itself", () => {
    // Regression: `inert` blocks a real browser from ever dispatching a
    // pointer event to the element it's set on (or descendants) — a handler
    // placed there is unreachable by any real click while collapsed. The
    // capture handler must live on a non-inert ancestor; this is that
    // section, one level up from the editor div.
    render(<DisclosureHarness />);

    const editor = screen.getByTestId("real-editor").parentElement!;
    expect(editor).toHaveAttribute("inert");
    const section = editor.parentElement!;
    expect(section).toHaveAttribute("data-description-disclosure");
    expect(section).not.toHaveAttribute("inert");

    fireEvent.pointerDown(section);

    expect(editor).not.toHaveAttribute("inert");
    expect(screen.getByRole("button", { name: "Show less" })).toBeInTheDocument();
  });

  it("does not hide or duplicate a short description", () => {
    measurement.current = {
      totalRows: 12,
      hiddenRows: 0,
      hasOverflow: false,
      lineHeight: 20,
      previewText: "Complete description",
    };
    render(<DisclosureHarness />);

    expect(screen.queryByRole("button", { name: /Show more|Show less/ })).not.toBeInTheDocument();
    expect(document.querySelector("[data-description-accessible-preview]")).toBeNull();
    expect(screen.getByTestId("real-editor").parentElement).not.toHaveAttribute("aria-hidden");
  });

  it("keeps a known short description visible while its measurement refreshes", () => {
    measurement.current = {
      totalRows: 12,
      hiddenRows: 0,
      hasOverflow: false,
      lineHeight: 20,
      previewText: "Complete description",
    };
    let queuedFrame: FrameRequestCallback | null = null;
    vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => {
      queuedFrame = callback;
      return 1;
    });

    render(<DisclosureHarness />);
    act(() => TestResizeObserver.callback?.([], {} as ResizeObserver));

    expect(screen.queryByRole("button", { name: /Show more|Show less/ })).not.toBeInTheDocument();
    expect(screen.getByTestId("real-editor").parentElement).not.toHaveAttribute("aria-hidden");

    act(() => queuedFrame?.(0));
    expect(screen.queryByRole("button", { name: /Show more|Show less/ })).not.toBeInTheDocument();
  });
});
