// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, screen } from "@testing-library/react";
import { useInboxFilterStore } from "@multica/core/inbox/filter-store";
import { renderWithI18n } from "../../test/i18n";
import { InboxQuickFilters } from "./inbox-quick-filters";

const filtersOf = () => useInboxFilterStore.getState().filtersByWorkspace["ws-1"];
const pressed = () =>
  screen
    .getAllByRole("button")
    .filter((button) => button.getAttribute("aria-pressed") === "true")
    .map((button) => button.textContent);

beforeEach(() => useInboxFilterStore.setState({ filtersByWorkspace: {} }));
afterEach(cleanup);

describe("InboxQuickFilters", () => {
  it("renders All, Unread and Direct messages with All active by default", () => {
    renderWithI18n(<InboxQuickFilters wsId="ws-1" />);

    expect(
      screen.getAllByRole("button").map((button) => button.textContent),
    ).toEqual(["All", "Unread", "Direct messages"]);
    expect(pressed()).toEqual(["All"]);
  });

  it("binds Unread to unreadOnly and Direct messages to dmOnly, one at a time", () => {
    renderWithI18n(<InboxQuickFilters wsId="ws-1" />);

    fireEvent.click(screen.getByRole("button", { name: "Unread" }));
    expect(filtersOf()).toMatchObject({ unreadOnly: true, dmOnly: false });
    expect(pressed()).toEqual(["Unread"]);

    fireEvent.click(screen.getByRole("button", { name: "Direct messages" }));
    expect(filtersOf()).toMatchObject({ unreadOnly: false, dmOnly: true });
    expect(pressed()).toEqual(["Direct messages"]);
  });

  it("All clears both chips but keeps the filter-menu selections", () => {
    useInboxFilterStore.getState().togglePriorityFilter("ws-1", "high");
    renderWithI18n(<InboxQuickFilters wsId="ws-1" />);
    fireEvent.click(screen.getByRole("button", { name: "Direct messages" }));

    fireEvent.click(screen.getByRole("button", { name: "All" }));

    expect(filtersOf()).toMatchObject({
      unreadOnly: false,
      dmOnly: false,
      priorities: ["high"],
    });
    expect(pressed()).toEqual(["All"]);
  });

  it("follows the menu's unread switch", () => {
    useInboxFilterStore.getState().toggleUnreadOnly("ws-1");
    renderWithI18n(<InboxQuickFilters wsId="ws-1" />);
    expect(pressed()).toEqual(["Unread"]);
  });
});
