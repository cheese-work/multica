import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useCommentDraftStore } from "@multica/core/issues/stores";
import { renderWithI18n } from "../../test/i18n";
import { useCommentAnnotations } from "./use-comment-annotations";

vi.mock("sonner", () => ({ toast: { error: vi.fn() } }));

const key = "reply:issue:root" as const;

const hostRenders = vi.fn();

// Measured on a real iPhone (iOS Safari, CHE-2077 capture, 390pt-wide viewport): the native
// Copy | Look Up | Translate menu spans 23..78px above the selection top, and the end-handle
// knob hangs about 20px below the selection bottom.
const NATIVE_MENU_TOP_ABOVE_SELECTION = 78;
const SELECTION_HANDLE_BELOW_SELECTION = 20;

function Fixture() {
  hostRenders();
  const annotation = useCommentAnnotations({ draftKey: key, sources: [{ id: "root", name: "agent" }], enabled: true });
  return <div ref={annotation.cardRef} {...annotation.captureProps}>
    {annotation.popup}
    <div data-comment-content="root" tabIndex={0}>Selected text</div>
  </div>;
}

function selectText(container: HTMLElement) {
  const source = container.querySelector<HTMLElement>("[data-comment-content]")!;
  fireEvent.pointerDown(source, { pointerType: "mouse", button: 0 });
  const range = document.createRange();
  range.selectNodeContents(source);
  act(() => { window.getSelection()!.removeAllRanges(); window.getSelection()!.addRange(range); });
  fireEvent.pointerUp(source);
}

function setPointer(coarse: boolean) {
  Object.defineProperty(window, "matchMedia", {
    configurable: true,
    value: (query: string) => ({ matches: query.includes("coarse") ? coarse : !coarse, media: query }),
  });
}

beforeEach(() => {
  useCommentDraftStore.setState({ drafts: {} });
  Object.defineProperty(document.documentElement, "clientWidth", { configurable: true, value: 1024 });
  Object.defineProperty(document.documentElement, "clientHeight", { configurable: true, value: 768 });
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
    return this.hasAttribute("data-reply-annotation-overlay") ? new DOMRect(0, 0, 100, 30) : new DOMRect(10, 10, 600, 600);
  });
  // Floating UI measures the popup with offsetWidth/offsetHeight.
  vi.spyOn(HTMLElement.prototype, "offsetWidth", "get").mockReturnValue(100);
  vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockReturnValue(30);
  Range.prototype.getBoundingClientRect = vi.fn(() => new DOMRect(10, 300, 120, 20));
  Range.prototype.getClientRects = vi.fn(() => [] as unknown as DOMRectList);
});
afterEach(() => { vi.restoreAllMocks(); });

describe("annotation note on iOS", () => {
  it("types a Vietnamese Telex sentence without rerendering the host per keystroke", async () => {
    setPointer(false);
    hostRenders.mockClear();
    const { container } = renderWithI18n(<Fixture />);
    selectText(container);
    fireEvent.click(await screen.findByRole("button", { name: "Add to reply" }));
    const input = await screen.findByRole("textbox", { name: "Comment" });
    const count = hostRenders.mock.calls.length;
    // Telex: each keystroke replaces the composed run, as iOS reports it.
    fireEvent.compositionStart(input);
    for (const text of ["T", "Ti", "Tiê", "Tiế", "Tiếng", "Tiếng V", "Tiếng Vi", "Tiếng Việ", "Tiếng Việt"]) {
      fireEvent.change(input, { target: { value: text } });
    }
    fireEvent.compositionEnd(input);
    expect(hostRenders.mock.calls.length).toBe(count);
    expect(input).toHaveValue("Tiếng Việt");
    fireEvent.click(screen.getByRole("button", { name: "Add annotation" }));
    await waitFor(() => expect(useCommentDraftStore.getState().getAnnotations(key)[0]?.note).toBe("Tiếng Việt"));
  });

  it("places the popup below the selection on a touch device so the system menu above it cannot cover it", async () => {
    setPointer(true);
    const { container } = renderWithI18n(<Fixture />);
    selectText(container);
    const add = await screen.findByRole("button", { name: "Add to reply" });
    const overlay = add.closest<HTMLElement>("[data-reply-annotation-overlay]")!;
    await waitFor(() => expect(overlay.style.visibility).toBe("visible"));
    // Selection bottom is 320; the popup must clear the end-handle knob.
    expect(parseFloat(overlay.style.top)).toBeGreaterThanOrEqual(320 + SELECTION_HANDLE_BELOW_SELECTION);
  });

  it("never overlaps the selected text on a touch device when there is no room below", async () => {
    setPointer(true);
    // Selection at 745..765 in a 768px viewport: a 30px popup does not fit below it.
    Range.prototype.getBoundingClientRect = vi.fn(() => new DOMRect(10, 745, 120, 20));
    const { container } = renderWithI18n(<Fixture />);
    selectText(container);
    const add = await screen.findByRole("button", { name: "Add to reply" });
    const overlay = add.closest<HTMLElement>("[data-reply-annotation-overlay]")!;
    await waitFor(() => expect(overlay.style.visibility).toBe("visible"));
    const top = parseFloat(overlay.style.top);
    // Falls back to above the selection, but above the native menu stacked there too:
    // popup bottom stays clear of the measured top edge of the native menu.
    expect(top + 30).toBeLessThanOrEqual(745 - NATIVE_MENU_TOP_ABOVE_SELECTION);
  });

  it("never slides vertically into the selection or native menu in a constrained viewport on touch", async () => {
    setPointer(true);
    // 100px viewport: neither below (60+28+30 > 100) nor above (40-88 < 0) fits, so a vertical
    // shift would drag the popup into the selection (40..60), its handle, or the native menu.
    Object.defineProperty(document.documentElement, "clientHeight", { configurable: true, value: 100 });
    Range.prototype.getBoundingClientRect = vi.fn(() => new DOMRect(10, 40, 120, 20));
    const { container } = renderWithI18n(<Fixture />);
    selectText(container);
    const add = await screen.findByRole("button", { name: "Add to reply" });
    const overlay = add.closest<HTMLElement>("[data-reply-annotation-overlay]")!;
    await waitFor(() => expect(overlay.style.visibility).toBe("visible"));
    const top = parseFloat(overlay.style.top);
    const bottom = top + 30;
    const belowClear = top >= 60 + SELECTION_HANDLE_BELOW_SELECTION;
    const aboveClear = bottom <= 40 - NATIVE_MENU_TOP_ABOVE_SELECTION;
    expect(belowClear || aboveClear).toBe(true);
  });

  it("keeps the popup above the selection with a mouse", async () => {
    setPointer(false);
    const { container } = renderWithI18n(<Fixture />);
    selectText(container);
    const add = await screen.findByRole("button", { name: "Add to reply" });
    const overlay = add.closest<HTMLElement>("[data-reply-annotation-overlay]")!;
    await waitFor(() => expect(overlay.style.visibility).toBe("visible"));
    expect(parseFloat(overlay.style.top)).toBeLessThan(300);
  });
});
