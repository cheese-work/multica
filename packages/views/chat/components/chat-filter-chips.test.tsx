// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import type { ChatListView } from "@multica/core/chat/queries";
import enChat from "../../locales/en/chat.json";
import { ChatFilterChips } from "./chat-filter-chips";

const RES = { en: { chat: enChat } };

function setup(value: ChatListView = "main", counts = { decisions: 2, waiting: 7, unseen: 5, archived: 38 }) {
  const onChange = vi.fn();
  render(
    <I18nProvider locale="en" resources={RES}>
      <div>
        <ChatFilterChips value={value} counts={counts} onChange={onChange} />
        <div data-testid="list" />
      </div>
    </I18nProvider>,
  );
  const toolbar = screen.getByRole("toolbar", { name: "Filter chats" });
  const track = toolbar.querySelector<HTMLElement>("[data-chip-track]") ?? toolbar;
  return { onChange, toolbar, track };
}

function overflow(track: HTMLElement, { scrollWidth, clientWidth }: { scrollWidth: number; clientWidth: number }) {
  Object.defineProperty(track, "scrollWidth", { configurable: true, value: scrollWidth });
  Object.defineProperty(track, "clientWidth", { configurable: true, value: clientWidth });
}

describe("ChatFilterChips", () => {
  it("lists the chips in order with Main selected and accessible names carrying the count", () => {
    setup();
    const chips = screen.getAllByRole("button");
    expect(chips.map((c) => c.getAttribute("aria-label") ?? c.textContent)).toEqual([
      "All",
      "Main",
      "Decisions, 2",
      "Waiting on you, 7",
      "Unseen, 5",
      "Pinned",
      "Archived, 38",
    ]);
    expect(screen.getByRole("button", { name: "Main" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: "All" })).toHaveAttribute("aria-pressed", "false");
  });

  it("hides a count while it is unknown instead of showing 0", () => {
    setup("main", {} as never);
    expect(screen.getByRole("button", { name: "Decisions" })).toBeInTheDocument();
  });

  it("selecting a chip reports its view; the pin chip cycles Pinned → Not pinned", () => {
    const { onChange } = setup();
    fireEvent.click(screen.getByRole("button", { name: "Waiting on you, 7" }));
    expect(onChange).toHaveBeenLastCalledWith("waiting");
    fireEvent.click(screen.getByRole("button", { name: "Pinned" }));
    expect(onChange).toHaveBeenLastCalledWith("pinned");
  });

  it("the pin chip reads Not pinned and flips back when that view is selected", () => {
    const { onChange } = setup("unpinned");
    const chip = screen.getByRole("button", { name: "Not pinned" });
    expect(chip).toHaveAttribute("aria-pressed", "true");
    fireEvent.click(chip);
    expect(onChange).toHaveBeenLastCalledWith("pinned");
  });

  it("roving focus: one tab stop; arrows, Home and End move focus", () => {
    setup();
    const chips = screen.getAllByRole("button");
    expect(chips.filter((c) => c.tabIndex === 0)).toHaveLength(1);
    expect(screen.getByRole("button", { name: "Main" }).tabIndex).toBe(0);
    screen.getByRole("button", { name: "Main" }).focus();
    fireEvent.keyDown(document.activeElement!, { key: "ArrowRight" });
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Decisions, 2" }));
    expect(screen.getByRole("button", { name: "Decisions, 2" }).tabIndex).toBe(0);
    fireEvent.keyDown(document.activeElement!, { key: "ArrowLeft" });
    fireEvent.keyDown(document.activeElement!, { key: "ArrowLeft" });
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "All" }));
    fireEvent.keyDown(document.activeElement!, { key: "ArrowLeft" });
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Archived, 38" }));
    fireEvent.keyDown(document.activeElement!, { key: "Home" });
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "All" }));
    fireEvent.keyDown(document.activeElement!, { key: "End" });
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Archived, 38" }));
  });

  it("wheel over an overflowing row scrolls it sideways and keeps the page still", () => {
    const { track } = setup();
    overflow(track, { scrollWidth: 600, clientWidth: 300 });
    track.scrollLeft = 10;
    const notCancelled = fireEvent.wheel(track, { deltaY: 40 });
    expect(track.scrollLeft).toBe(50);
    expect(notCancelled).toBe(false); // preventDefault called: list does not scroll
  });

  it("at the row end the wheel passes through to the list", () => {
    const { track } = setup();
    overflow(track, { scrollWidth: 600, clientWidth: 300 });
    track.scrollLeft = 300;
    const notCancelled = fireEvent.wheel(track, { deltaY: 40 });
    expect(notCancelled).toBe(true);
    expect(track.scrollLeft).toBe(300);
  });

  it("at the row start an upward wheel passes through too", () => {
    const { track } = setup();
    overflow(track, { scrollWidth: 600, clientWidth: 300 });
    track.scrollLeft = 0;
    expect(fireEvent.wheel(track, { deltaY: -40 })).toBe(true);
  });

  it("touchpad horizontal scroll is left to the browser", () => {
    const { track } = setup();
    overflow(track, { scrollWidth: 600, clientWidth: 300 });
    track.scrollLeft = 10;
    expect(fireEvent.wheel(track, { deltaX: 40, deltaY: 2 })).toBe(true);
    expect(track.scrollLeft).toBe(10);
  });

  it("when every chip fits, the wheel is the list's and nothing is faded", () => {
    const { track, toolbar } = setup();
    overflow(track, { scrollWidth: 300, clientWidth: 300 });
    expect(fireEvent.wheel(track, { deltaY: 40 })).toBe(true);
    expect(toolbar).not.toHaveAttribute("data-overflowing", "true");
  });
});
