import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useCommentDraftStore } from "@multica/core/issues/stores";
import type { ReplyAnnotation } from "@multica/core/drafts/reply-annotation";
import { renderWithI18n } from "../../test/i18n";
import { ChatAnnotationChip } from "./chat-annotations";

const draftKey = "chat:s1" as const;
const make = (n: number): ReplyAnnotation => ({
  id: `a${n}`, sourceCommentId: "m1", sourceActorName: "Terra", quote: `Quote ${n}`, note: `Note ${n}`,
  start: n * 10, prefix: "", suffix: "",
});

function Fixture({ onEdit = () => true, onActive, onEmpty, disabled = false }: {
  onEdit?: (id: string) => boolean; onActive?: (id: string | null) => void; onEmpty?: () => void; disabled?: boolean;
}) {
  const annotations = useCommentDraftStore((s) => s.getAnnotations(draftKey));
  if (!annotations.length) return <button type="button">Composer</button>;
  return <ChatAnnotationChip draftKey={draftKey} annotations={annotations} disabled={disabled}
    onEditAnnotation={onEdit} onActiveChange={onActive} onEmpty={onEmpty} />;
}

function seed(count: number) {
  useCommentDraftStore.setState({ drafts: {} });
  for (let i = 1; i <= count; i++) useCommentDraftStore.getState().addAnnotation(draftKey, make(i));
}

beforeEach(() => seed(2));

describe("DM comment list", () => {
  it("opens from the keyboard and gives every row a visible Edit and Remove button", async () => {
    const user = userEvent.setup();
    renderWithI18n(<Fixture />);
    await user.tab();
    expect(screen.getByRole("button", { name: "2 comments" })).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(await screen.findByText("Quote 1")).toBeVisible();
    expect(screen.getByText("Note 2")).toBeVisible();
    for (const n of [1, 2]) {
      expect(screen.getByRole("button", { name: `Edit comment ${n}` })).toBeVisible();
      expect(screen.getByRole("button", { name: `Remove comment ${n}` })).toBeVisible();
    }
  });

  it("tabs through the scroll area, then Edit and Remove of each row in order", async () => {
    const user = userEvent.setup();
    renderWithI18n(<Fixture />);
    await user.tab();
    await user.keyboard("{Enter}");
    await screen.findByText("Quote 1");
    const order = ["Comments", "Edit comment 1", "Remove comment 1", "Edit comment 2", "Remove comment 2"];
    for (const name of order) {
      await user.tab();
      expect(document.activeElement).toHaveAccessibleName(name);
    }
  });

  it("Escape closes the list and returns focus to the count", async () => {
    const user = userEvent.setup();
    renderWithI18n(<Fixture />);
    await user.tab();
    await user.keyboard("{Enter}");
    await screen.findByText("Quote 1");
    await user.tab();
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByText("Quote 1")).not.toBeInTheDocument());
    expect(screen.getByRole("button", { name: "2 comments" })).toHaveFocus();
  });

  it("Edit opens the note at the marker and closes the list", async () => {
    const onEdit = vi.fn(() => true);
    renderWithI18n(<Fixture onEdit={onEdit} />);
    fireEvent.click(screen.getByRole("button", { name: "2 comments" }));
    fireEvent.click(await screen.findByRole("button", { name: "Edit comment 2" }));
    expect(onEdit).toHaveBeenCalledWith("a2");
    await waitFor(() => expect(screen.queryByText("Quote 1")).not.toBeInTheDocument());
  });

  it("keeps the row and says why when the source is gone", async () => {
    renderWithI18n(<Fixture onEdit={() => false} />);
    fireEvent.click(screen.getByRole("button", { name: "2 comments" }));
    fireEvent.click(await screen.findByRole("button", { name: "Edit comment 2" }));
    expect(screen.getByText("The source changed or is unavailable. Your saved quote is kept.")).toHaveAttribute("role", "status");
    expect(screen.getByText("Note 2")).toBeVisible();
    expect(screen.getByRole("button", { name: "Remove comment 2" })).toBeEnabled();
  });

  it("Remove moves focus to the next row's Edit, then the previous one, and announces it", async () => {
    const user = userEvent.setup();
    seed(3);
    renderWithI18n(<Fixture />);
    await user.tab();
    await user.keyboard("{Enter}");
    await screen.findByText("Quote 1");
    fireEvent.click(screen.getByRole("button", { name: "Remove comment 2" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Edit comment 2" })).toHaveFocus());
    expect(screen.getAllByRole("status").map((el) => el.textContent).join(" ")).toContain("Comment 2 removed");
    expect(screen.getByText("Quote 3")).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "Remove comment 2" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Edit comment 1" })).toHaveFocus());
    expect(useCommentDraftStore.getState().getAnnotations(draftKey).map((a) => a.id)).toEqual(["a1"]);
  });

  it("hands focus to the composer when the last comment goes", async () => {
    const onEmpty = vi.fn();
    seed(1);
    renderWithI18n(<Fixture onEmpty={onEmpty} />);
    fireEvent.click(screen.getByRole("button", { name: "1 comment" }));
    fireEvent.click(await screen.findByRole("button", { name: "Remove comment 1" }));
    expect(onEmpty).toHaveBeenCalledTimes(1);
  });

  it("lights the matching marker while a row is hovered or focused", async () => {
    const onActive = vi.fn();
    renderWithI18n(<Fixture onActive={onActive} />);
    fireEvent.click(screen.getByRole("button", { name: "2 comments" }));
    const row = (await screen.findByText("Quote 2")).closest("[data-testid='chat-annotation-row']")!;
    fireEvent.mouseEnter(row);
    expect(onActive).toHaveBeenLastCalledWith("a2");
    fireEvent.mouseLeave(row);
    expect(onActive).toHaveBeenLastCalledWith(null);
    fireEvent.focus(screen.getByRole("button", { name: "Edit comment 1" }));
    expect(onActive).toHaveBeenLastCalledWith("a1");
  });

  it("locks the count button while the composer is locked", () => {
    renderWithI18n(<Fixture disabled />);
    expect(screen.getByRole("button", { name: "2 comments" })).toBeDisabled();
  });
});

describe("DM comment list with 20 comments", () => {
  beforeEach(() => {
    seed(20);
    // jsdom has no layout: 56 px rows, 8 px gap, a 272 px scroll area.
    vi.spyOn(HTMLElement.prototype, "offsetTop", "get").mockImplementation(function (this: HTMLElement) {
      return Number(this.dataset.index ?? 0) * 64;
    });
    vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockImplementation(function (this: HTMLElement) {
      return this.dataset.testid === "chat-annotation-row" || this.dataset.index ? 56 : 0;
    });
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockImplementation(function (this: HTMLElement) {
      return this.dataset.testid === "chat-annotation-scroll" ? 272 : 0;
    });
    vi.spyOn(HTMLElement.prototype, "scrollHeight", "get").mockImplementation(function (this: HTMLElement) {
      return this.dataset.testid === "chat-annotation-scroll" ? 20 * 64 - 8 : 0;
    });
  });

  it("keeps all 20 comments reachable, each with Edit and Remove", async () => {
    renderWithI18n(<Fixture />);
    fireEvent.click(screen.getByRole("button", { name: "20 comments" }));
    await screen.findByText("Quote 1");
    for (const n of [1, 10, 20]) {
      expect(screen.getByRole("button", { name: `Edit comment ${n}` })).toBeInTheDocument();
      expect(screen.getByRole("button", { name: `Remove comment ${n}` })).toBeInTheDocument();
    }
    expect(screen.getAllByTestId("chat-annotation-row")).toHaveLength(20);
    expect(screen.getByTestId("chat-annotation-scroll")).toHaveAttribute("tabindex", "0");
  });

  it("shows which comments are in view and follows the scroll to the end", async () => {
    renderWithI18n(<Fixture />);
    fireEvent.click(screen.getByRole("button", { name: "20 comments" }));
    const scroll = await screen.findByTestId("chat-annotation-scroll");
    await waitFor(() => expect(screen.getByTestId("chat-annotation-range")).toHaveTextContent("Comments 1–4 of 20"));
    scroll.scrollTop = 20 * 64 - 8 - 272;
    act(() => { fireEvent.scroll(scroll); });
    expect(screen.getByTestId("chat-annotation-range")).toHaveTextContent("Comments 17–20 of 20");
  });

  it("scrolls a row into view when Tab lands on its Edit button", async () => {
    const scrollIntoView = vi.fn();
    Element.prototype.scrollIntoView = scrollIntoView;
    renderWithI18n(<Fixture />);
    fireEvent.click(screen.getByRole("button", { name: "20 comments" }));
    fireEvent.focus(await screen.findByRole("button", { name: "Edit comment 20" }));
    expect(scrollIntoView).toHaveBeenCalledWith({ block: "nearest" });
  });
});
