import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { useCommentDraftStore } from "@multica/core/issues/stores";
import { renderWithI18n } from "../../test/i18n";
import { useCommentAnnotations, type AnnotationLabels } from "./use-comment-annotations";

vi.mock("sonner", () => ({ toast: { error: vi.fn() } }));

const key = "chat:s1" as const;
const labels: AnnotationLabels = {
  selectionFailed: "Could not capture the selection.",
  quoteLimit: (count) => `Select up to ${count} characters at a time.`,
  countLimit: (count) => `A message can contain up to ${count} comments.`,
  add: "Add comment", confirmAdd: "Add", save: "Save",
  noteLabel: (number) => `Comment ${number}`, notePlaceholder: "Add a comment…",
  edit: (number, note) => `Edit comment ${number}: ${note}`,
  remove: (number) => `Remove comment ${number}`,
};

function ChatFixture({ surface = "chat" }: { surface?: "chat" | "issue" }) {
  const firstId = useCommentDraftStore((s) => s.getAnnotations(key)[0]?.id);
  const annotation = useCommentAnnotations({
    draftKey: key, sources: [{ id: "m1", name: "Terra" }], enabled: true, surface,
    labels: surface === "chat" ? labels : undefined,
  });
  return <div ref={annotation.cardRef} {...annotation.captureProps}>
    {annotation.popup}
    <div data-annotation-overlay-host>
      <div data-comment-content="m1" tabIndex={0} role="group" aria-label="Message from Terra">Selected text</div>
      <div data-annotation-overlay-slot data-testid="slot" />
      <button type="button">Copy</button>
    </div>
    <button type="button" onClick={() => annotation.setActiveId(firstId ?? null)}>Light row</button>
  </div>;
}

function selectText(container: HTMLElement) {
  const source = container.querySelector<HTMLElement>("[data-comment-content]")!;
  fireEvent.pointerDown(source, { pointerType: "mouse", button: 0 });
  const range = document.createRange();
  range.selectNodeContents(source);
  act(() => {
    window.getSelection()!.removeAllRanges();
    window.getSelection()!.addRange(range);
  });
  fireEvent.pointerUp(source);
  return source;
}

async function addComment(container: HTMLElement, note: string) {
  selectText(container);
  fireEvent.click(await screen.findByRole("button", { name: "Add comment" }));
  fireEvent.change(await screen.findByRole("textbox", { name: "Comment 1" }), { target: { value: note } });
  fireEvent.click(screen.getByRole("button", { name: "Add" }));
}

beforeEach(() => {
  useCommentDraftStore.setState({ drafts: {} });
  Object.defineProperty(document.documentElement, "clientWidth", { configurable: true, value: 1024 });
  Object.defineProperty(document.documentElement, "clientHeight", { configurable: true, value: 768 });
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue(new DOMRect(10, 10, 600, 400));
  Range.prototype.getBoundingClientRect = vi.fn(() => new DOMRect(10, 10, 120, 20));
  Range.prototype.getClientRects = vi.fn(() => [] as unknown as DOMRectList);
});

describe("chat surface reading and focus order", () => {
  it("names controls from the labels and puts the marker in the message's slot", async () => {
    const { container } = renderWithI18n(<ChatFixture />);
    await addComment(container, "Why 5 s?");
    const marker = await screen.findByRole("button", { name: "Edit comment 1: Why 5 s?" });
    expect(marker).toHaveAttribute("data-testid", "annotation-marker");
    expect(screen.getByTestId("slot")).toContainElement(marker);
  });

  it("tabs from the message to its marker, then to the next control", async () => {
    const user = userEvent.setup();
    const { container } = renderWithI18n(<ChatFixture />);
    await addComment(container, "Why 5 s?");
    const marker = await screen.findByRole("button", { name: "Edit comment 1: Why 5 s?" });
    const message = screen.getByRole("group", { name: "Message from Terra" });
    message.focus();
    await user.tab();
    expect(marker).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("button", { name: "Copy" })).toHaveFocus();
    expect(message.compareDocumentPosition(marker) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("moves focus to the new marker after Add, and back to it after Esc on an edit", async () => {
    const { container } = renderWithI18n(<ChatFixture />);
    await addComment(container, "Why 5 s?");
    const marker = await screen.findByRole("button", { name: "Edit comment 1: Why 5 s?" });
    await waitFor(() => expect(marker).toHaveFocus());
    fireEvent.click(marker);
    const note = await screen.findByRole("textbox", { name: "Comment 1" });
    expect(screen.getByText("Comment 1", { selector: "p" })).toBeInTheDocument();
    fireEvent.keyDown(note, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("textbox")).not.toBeInTheDocument());
    expect(screen.getByRole("button", { name: "Edit comment 1: Why 5 s?" })).toHaveFocus();
  });

  it("returns focus to the message after Esc on a new comment", async () => {
    const { container } = renderWithI18n(<ChatFixture />);
    selectText(container);
    fireEvent.click(await screen.findByRole("button", { name: "Add comment" }));
    fireEvent.keyDown(await screen.findByRole("textbox", { name: "Comment 1" }), { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("textbox")).not.toBeInTheDocument());
    expect(screen.getByRole("group", { name: "Message from Terra" })).toHaveFocus();
  });

  it("marks the marker active on hover and when a list row lights it", async () => {
    const { container } = renderWithI18n(<ChatFixture />);
    await addComment(container, "Why 5 s?");
    const marker = await screen.findByRole("button", { name: "Edit comment 1: Why 5 s?" });
    await waitFor(() => expect(marker).toHaveFocus());
    expect(marker).toHaveAttribute("data-active", "true");
    act(() => marker.blur());
    expect(marker).not.toHaveAttribute("data-active", "true");
    fireEvent.mouseEnter(marker);
    expect(marker).toHaveAttribute("data-active", "true");
    fireEvent.mouseLeave(marker);
    expect(marker).not.toHaveAttribute("data-active", "true");
    fireEvent.click(screen.getByRole("button", { name: "Light row" }));
    expect(marker).toHaveAttribute("data-active", "true");
  });

  it("keeps the issue surface portalling markers to the page body", async () => {
    const { container } = renderWithI18n(<ChatFixture surface="issue" />);
    selectText(container);
    fireEvent.click(await screen.findByRole("button", { name: "Add to reply" }));
    fireEvent.change(await screen.findByRole("textbox", { name: "Comment" }), { target: { value: "N" } });
    fireEvent.click(screen.getByRole("button", { name: "Add annotation" }));
    const marker = await screen.findByRole("button", { name: "Edit annotation 1" });
    expect(screen.getByTestId("slot")).not.toContainElement(marker);
    expect(marker.closest("[data-reply-annotation-overlay]")?.parentElement).toBe(document.body);
  });
});
