import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import * as floatingUi from "@floating-ui/dom";
import { useCommentDraftStore } from "@multica/core/issues/stores";
import { renderWithI18n } from "../../test/i18n";
import { useCommentAnnotations, type AnnotationLabels } from "./use-comment-annotations";

vi.mock("sonner", () => ({ toast: { error: vi.fn() } }));
vi.mock("@floating-ui/dom", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@floating-ui/dom")>();
  return { ...actual, computePosition: vi.fn(actual.computePosition) };
});

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

beforeEach(async () => {
  const actual = await vi.importActual<typeof floatingUi>("@floating-ui/dom");
  vi.mocked(floatingUi.computePosition).mockReset().mockImplementation(actual.computePosition);
  useCommentDraftStore.setState({ drafts: {} });
  Object.defineProperty(document.documentElement, "clientWidth", { configurable: true, value: 1024 });
  Object.defineProperty(document.documentElement, "clientHeight", { configurable: true, value: 768 });
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue(new DOMRect(10, 10, 600, 400));
  Range.prototype.getBoundingClientRect = vi.fn(() => new DOMRect(10, 10, 120, 20));
  Range.prototype.getClientRects = vi.fn(() => [] as unknown as DOMRectList);
});
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe("selection released outside the message text", () => {
  function select(source: HTMLElement) {
    const range = document.createRange();
    range.selectNodeContents(source);
    act(() => { window.getSelection()!.removeAllRanges(); window.getSelection()!.addRange(range); });
  }

  it.each(["card padding", "document body"])("opens the popup when pointerup lands on the %s", async (where) => {
    const { container } = renderWithI18n(<ChatFixture />);
    const source = container.querySelector<HTMLElement>("[data-comment-content]")!;
    fireEvent.pointerDown(source, { pointerType: "mouse", button: 0 });
    select(source);
    fireEvent.pointerUp(where === "card padding" ? container.firstElementChild! : document.body);
    expect(await screen.findByRole("button", { name: "Add comment" })).toBeVisible();
  });

  it("ignores a pointerup that has no armed pointerdown in the message", () => {
    const { container } = renderWithI18n(<ChatFixture />);
    select(container.querySelector<HTMLElement>("[data-comment-content]")!);
    fireEvent.pointerUp(document.body);
    expect(screen.queryByRole("button", { name: "Add comment" })).toBeNull();
  });

  it("opens only once per press", async () => {
    const { container } = renderWithI18n(<ChatFixture />);
    const source = container.querySelector<HTMLElement>("[data-comment-content]")!;
    fireEvent.pointerDown(source, { pointerType: "mouse", button: 0 });
    select(source);
    fireEvent.pointerUp(document.body);
    fireEvent.click(await screen.findByRole("button", { name: "Add comment" }));
    window.getSelection()!.removeAllRanges();
    select(source);
    fireEvent.pointerUp(document.body);
    expect(screen.queryByRole("button", { name: "Add comment" })).toBeNull();
  });
});

describe("chat surface reading and focus order", () => {
  it.each(["fine", "coarse"])("focuses once per open and ignores a previous comment's late marker positioning (%s pointer)", async (pointer) => {
    vi.spyOn(window, "matchMedia").mockImplementation((query) => ({ matches: query === `(pointer: ${pointer})` }) as MediaQueryList);
    const focus = vi.spyOn(HTMLTextAreaElement.prototype, "focus");
    const { computePosition } = await vi.importActual<typeof floatingUi>("@floating-ui/dom");
    const markerPositions: (() => void)[] = [];
    let delayMarkers = true;
    vi.mocked(floatingUi.computePosition).mockImplementation(async (reference, floating, options) => {
      const position = await computePosition(reference, floating, options);
      if (delayMarkers && floating.querySelector("[data-annotation-marker]")) {
        await new Promise<void>((resolve) => { markerPositions.push(resolve); });
      }
      return position;
    });
    const { container } = renderWithI18n(<ChatFixture />);
    selectText(container);
    fireEvent.click(await screen.findByRole("button", { name: "Add comment" }));
    const first = await screen.findByRole("textbox", { name: "Comment 1" });
    await waitFor(() => expect(first).toBeVisible());
    if (pointer === "coarse") act(() => first.focus());
    await waitFor(() => expect(first).toHaveFocus());
    expect(focus).toHaveBeenCalledTimes(1);
    fireEvent.change(first, { target: { value: "First note" } });
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    await waitFor(() => expect(markerPositions.length).toBeGreaterThan(0));

    const source = selectText(container);
    const range = document.createRange();
    range.setStart(source.firstChild!, 9);
    range.setEnd(source.firstChild!, 13);
    fireEvent.pointerDown(source, { pointerType: "mouse", button: 0 });
    act(() => { window.getSelection()!.removeAllRanges(); window.getSelection()!.addRange(range); });
    fireEvent.pointerUp(source);
    fireEvent.click(await screen.findByRole("button", { name: "Add comment" }));
    const second = await screen.findByRole("textbox", { name: "Comment 2" });
    await waitFor(() => expect(second).toBeVisible());
    if (pointer === "coarse") act(() => second.focus());
    await waitFor(() => expect(second).toHaveFocus());
    expect(focus).toHaveBeenCalledTimes(2);
    delayMarkers = false;
    await act(async () => { markerPositions.splice(0).forEach((resolve) => resolve()); });
    expect(second).toHaveFocus();
    fireEvent(window, new Event("resize"));
    await act(async () => {});
    expect(focus).toHaveBeenCalledTimes(2);
    expect(second).toHaveFocus();
  });

  it.each(["resize", "scroll"])("keeps the same focused editor inside the visual viewport on %s", async (event) => {
    vi.spyOn(window, "matchMedia").mockImplementation((query) => ({ matches: query === "(pointer: coarse)" }) as MediaQueryList);
    const focus = vi.spyOn(HTMLTextAreaElement.prototype, "focus");
    const viewport = Object.assign(new EventTarget(), {
      width: 1024, height: 768, offsetTop: 0, offsetLeft: 0, scale: 1,
    });
    vi.stubGlobal("visualViewport", viewport);
    vi.spyOn(HTMLElement.prototype, "offsetWidth", "get").mockReturnValue(320);
    vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockReturnValue(120);
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
      return this.hasAttribute("data-reply-annotation-overlay")
        ? new DOMRect(0, 0, 320, 120) : new DOMRect(10, 10, 600, 700);
    });
    Range.prototype.getBoundingClientRect = vi.fn(() => new DOMRect(10, 300, 120, 20));
    const { container, unmount } = renderWithI18n(<ChatFixture />);
    selectText(container);
    fireEvent.click(await screen.findByRole("button", { name: "Add comment" }));
    const input = await screen.findByRole("textbox", { name: "Comment 1" });
    await waitFor(() => expect(input).toBeVisible());
    act(() => input.focus());
    await waitFor(() => expect(input).toHaveFocus());
    const overlay = input.closest<HTMLElement>("[data-reply-annotation-overlay]")!;
    viewport.height = event === "resize" ? 200 : 280;
    viewport.offsetTop = event === "scroll" ? 400 : 0;
    act(() => { viewport.dispatchEvent(new Event(event)); });
    await waitFor(() => {
      expect(input).toBeVisible();
      expect(parseFloat(overlay.style.top)).toBeGreaterThanOrEqual(viewport.offsetTop + 8);
      expect(parseFloat(overlay.style.top) + 120).toBeLessThanOrEqual(viewport.offsetTop + viewport.height - 8);
    });
    expect(screen.getByRole("textbox")).toBe(input);
    expect(input).toHaveFocus();
    expect(focus).toHaveBeenCalledTimes(1);
    expect(parseFloat(overlay.style.maxHeight)).toBe(viewport.height - 16);
    unmount();
    const update = vi.mocked(floatingUi.computePosition).mockClear();
    viewport.dispatchEvent(new Event(event));
    await act(async () => {});
    expect(update).not.toHaveBeenCalled();
  });

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
