import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { useAnnotationNoteDraftStore, useCommentDraftStore } from "@multica/core/issues/stores";
import { renderWithI18n } from "../../test/i18n";
import { useCommentAnnotations } from "./use-comment-annotations";

// The composer chunk is held back until the test releases it: the first open on a cold page.
const chunk = vi.hoisted(() => {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  return { gate, release };
});
vi.mock("./annotation-composer", async (original) => {
  const actual = await original<typeof import("./annotation-composer")>();
  await chunk.gate;
  return actual;
});
vi.mock("sonner", () => ({ toast: { error: vi.fn() } }));
vi.mock("../../editor/content-editor", async () => {
  const { forwardRef, useRef, useImperativeHandle } = await import("react");
  return { ContentEditor: forwardRef(function Editor({ ariaLabel }: { ariaLabel?: string }, ref) {
    const input = useRef<HTMLTextAreaElement>(null);
    useImperativeHandle(ref, () => ({
      getMarkdown: () => input.current?.value ?? "", focus: () => input.current?.focus(), hasActiveUploads: () => false,
    }));
    return <textarea ref={input} aria-label={ariaLabel} />;
  }) };
});

function Fixture() {
  const annotation = useCommentAnnotations({
    issueId: "issue", draftKey: "reply:issue:root", sources: [{ id: "root", name: "agent" }], enabled: true,
  });
  return <div ref={annotation.cardRef} {...annotation.captureProps}>
    {annotation.popup}
    {annotation.composer?.render()}
    <div data-comment-content="root" tabIndex={0}>Selected text</div>
  </div>;
}

beforeEach(() => {
  useCommentDraftStore.setState({ drafts: {} });
  useAnnotationNoteDraftStore.setState({ drafts: {} });
  Object.defineProperty(document.documentElement, "clientWidth", { configurable: true, value: 1024 });
  Object.defineProperty(document.documentElement, "clientHeight", { configurable: true, value: 768 });
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue(new DOMRect(10, 10, 600, 400));
  Range.prototype.getBoundingClientRect = vi.fn(() => new DOMRect(10, 10, 120, 20));
  Range.prototype.getClientRects = vi.fn(() => [] as unknown as DOMRectList);
  vi.spyOn(window, "matchMedia").mockReturnValue({ matches: true } as MediaQueryList);
});

describe("first open before the composer chunk has loaded", () => {
  it("shows a loading status, then focuses the editor once it mounts", async () => {
    const { container } = renderWithI18n(<Fixture />);
    const source = container.querySelector<HTMLElement>("[data-comment-content]")!;
    source.focus();
    fireEvent.keyDown(source, { key: "ArrowRight", shiftKey: true });
    const range = document.createRange();
    range.selectNodeContents(source);
    act(() => { window.getSelection()!.removeAllRanges(); window.getSelection()!.addRange(range); });
    fireEvent.keyUp(source, { key: "ArrowRight", shiftKey: true });
    const add = await screen.findByRole("button", { name: "Add to reply" });
    add.focus();
    fireEvent.click(add);

    // The bubble is positioned while the chunk is still loading.
    expect(await screen.findByRole("status")).toHaveTextContent("Loading");
    expect(screen.queryByRole("textbox", { name: "Comment" })).not.toBeInTheDocument();

    await act(async () => { chunk.release(); await import("./annotation-composer"); });
    const input = await screen.findByRole("textbox", { name: "Comment" }, { timeout: 10_000 });
    await waitFor(() => expect(input).toHaveFocus(), { timeout: 5_000 });
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });
});
