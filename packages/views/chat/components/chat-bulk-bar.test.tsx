// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import type { ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";
import { ChatBulkBar } from "./chat-bulk-bar";

const RES = { en: { chat: enChat } };
const s = (id: string, o: Partial<ChatSession> = {}) => ({ id, status: "active", has_unread: false, unread_count: 0, pinned: false, ...o }) as ChatSession;
// 2 unseen, 1 seen
const picked = [s("1", { has_unread: true }), s("2", { has_unread: true }), s("3")];

function setup(compact = false, sessions = picked) {
  const onAction = vi.fn();
  render(
    <I18nProvider locale="en" resources={RES}>
      <ChatBulkBar sessions={sessions} compact={compact} onAction={onAction} />
    </I18nProvider>,
  );
  return { onAction };
}

afterEach(cleanup);

describe("ChatBulkBar", () => {
  it("labels the primary buttons by the selection and applies them to the matching DMs", () => {
    const { onAction } = setup();
    expect(screen.getByRole("toolbar", { name: "Actions for 3 selected chats" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Mark seen" }));
    expect(onAction).toHaveBeenCalledWith("seen", ["1", "2"]);
    expect(screen.getByRole("button", { name: "Pin" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Archive" })).toBeInTheDocument();
  });

  it("uses the singular for one selected chat", () => {
    setup(false, [s("1")]);
    expect(screen.getByRole("toolbar", { name: "Actions for 1 selected chat" })).toBeInTheDocument();
  });

  it("the arrow shows the opposite action with its count and runs it", async () => {
    const { onAction } = setup();
    fireEvent.click(screen.getByRole("button", { name: "More: Mark unseen (1)" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Mark unseen (1)" }));
    expect(onAction).toHaveBeenCalledWith("unseen", ["3"]);
  });

  it("offers no delete anywhere", async () => {
    setup();
    for (const b of screen.getAllByRole("button")) {
      expect(b.getAttribute("aria-label") ?? b.textContent).not.toMatch(/delete/i);
    }
  });

  it("compact mode is icon-only with accessible names", () => {
    setup(true);
    const primary = screen.getByRole("button", { name: "Mark seen" });
    expect(primary.textContent?.trim()).toBe("");
    expect(screen.getByRole("button", { name: "Pin" }).textContent?.trim()).toBe("");
    expect(screen.getByRole("button", { name: "Archive" }).textContent?.trim()).toBe("");
    // 44x44 targets on the phone bar
    expect(primary.className).toMatch(/\bsize-11\b|\bh-11\b/);
  });

  it("disables a split whose action has nothing to apply to", () => {
    setup(false, [s("1", { status: "archived" })]);
    expect(screen.getByRole("button", { name: "Mark seen" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Pin" })).toBeDisabled();
  });
});
