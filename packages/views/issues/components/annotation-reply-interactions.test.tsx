import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { useState } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Editor } from "@tiptap/core";
import { useAnnotationNoteDraftStore, useCommentDraftStore } from "@multica/core/issues/stores";
import { renderWithI18n } from "../../test/i18n";
import { useCommentAnnotations } from "./use-comment-annotations";
import { toast } from "sonner";
import type { InvalidMentionRejection } from "../invalid-agent-mentions-copy";

vi.mock("@multica/core/api", async (original) => {
  const actual = await original<typeof import("@multica/core/api")>();
  return { ...actual, api: { ...actual.api,
    previewCommentTriggers: vi.fn(async () => ({ agents: [], blocked: [] })),
    listTasksByIssue: vi.fn(async () => []),
  } };
});

const writeText = vi.fn(async (_text: string) => {});

const draftKey = "reply:issue:root" as const;
function Fixture({ deleted = false, onSubmit = async () => false }: {
  deleted?: boolean; onSubmit?: (sourceId: string, content: string) => Promise<boolean | InvalidMentionRejection>;
}) {
  const [client] = useState(() => new QueryClient({ defaultOptions: { queries: { retry: false } } }));
  const annotations = useCommentAnnotations({ issueId: "issue", draftKey, enabled: true,
    sources: [{ id: "root", name: "Root" }, ...(!deleted ? [{ id: "child", name: "Child" }] : [])],
    reply: { onSubmit, steerByDefault: () => false },
  });
  return <QueryClientProvider client={client}><div ref={annotations.cardRef} {...annotations.captureProps}>
    {annotations.popup}
    <div data-comment-content="root" tabIndex={0}>Root text</div>
    {!deleted && <div data-comment-content="child" tabIndex={0}>Child text</div>}
  </div></QueryClientProvider>;
}

function selectText(container: HTMLElement, id: string) {
  const source = container.querySelector<HTMLElement>(`[data-comment-content="${id}"]`)!;
  fireEvent.pointerDown(source);
  const range = document.createRange();
  range.selectNodeContents(source);
  act(() => { window.getSelection()!.removeAllRanges(); window.getSelection()!.addRange(range); });
  fireEvent.pointerUp(source);
  return source;
}

async function typeNote(value: string) {
  const input = await screen.findByRole("textbox", { name: "Comment" });
  await act(async () => { (input as HTMLElement & { editor: Editor }).editor.commands.setContent(value); });
  await waitFor(() => expect(screen.getByRole("button", { name: "Reply" })).toBeEnabled());
  return input;
}

// The hook loads the composer on demand; warm the module so the first open is not bound by findBy's 1 s.
beforeAll(() => import("./annotation-composer"), 30_000);

beforeEach(() => {
  useCommentDraftStore.setState({ drafts: {} });
  useAnnotationNoteDraftStore.setState({ drafts: {} });
  writeText.mockClear();
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  Object.defineProperty(document.documentElement, "clientWidth", { configurable: true, value: 1024 });
  Object.defineProperty(document.documentElement, "clientHeight", { configurable: true, value: 768 });
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue(new DOMRect(10, 10, 600, 400));
  Range.prototype.getBoundingClientRect = function () {
    return this.collapsed ? new DOMRect() : new DOMRect(10, 10, 120, 20);
  };
  Range.prototype.getClientRects = function () {
    return [this.getBoundingClientRect()] as unknown as DOMRectList;
  };
});

describe("selection replies with the real ContentEditor", () => {
  it("sends through the real ContentEditor", async () => {
    const onSubmit = vi.fn(async () => true);
    const { container } = renderWithI18n(<Fixture onSubmit={onSubmit} />);
    selectText(container, "child");
    fireEvent.click(await screen.findByRole("button", { name: "Add to reply" }));
    await typeNote("Please revise");
    fireEvent.click(screen.getByRole("button", { name: "Reply" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledWith("child", "> Child text\n\nPlease revise", undefined, undefined));
  });

  // Fork rule (AGENTS.md State Rules): authored input persists until successful send or explicit discard.
  it.each(["dismiss", "unmount"] as const)("restores an unsent issue-composer note after %s", async (how) => {
    const first = renderWithI18n(<Fixture />);
    selectText(first.container, "child");
    fireEvent.click(await screen.findByRole("button", { name: "Add to reply" }));
    await typeNote("Survives the popup");
    if (how === "dismiss") {
      fireEvent.pointerDown(document.body);
      await waitFor(() => expect(screen.queryByRole("textbox", { name: "Comment" })).not.toBeInTheDocument());
    } else first.unmount();

    const second = how === "dismiss" ? first : renderWithI18n(<Fixture />);
    selectText(second.container, "child");
    fireEvent.click(await screen.findByRole("button", { name: "Add to reply" }));
    expect(await screen.findByRole("textbox", { name: "Comment" })).toHaveTextContent("Survives the popup");
  });

  // Retrospective (added after the merge fix, not an original red): a refused mention keeps the composer and its note.
  it("keeps the composer and note open when the send returns an invalid-mention rejection", async () => {
    const onSubmit = vi.fn(async () => ({ invalidMentionIds: ["agent-1"] }));
    const view = renderWithI18n(<Fixture onSubmit={onSubmit} />);
    selectText(view.container, "child");
    fireEvent.click(await screen.findByRole("button", { name: "Add to reply" }));
    await typeNote("Mentions a bad agent");
    fireEvent.click(screen.getByRole("button", { name: "Reply" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByRole("button", { name: "Reply" })).toBeEnabled());
    expect(screen.getByRole("textbox", { name: "Comment" })).toHaveTextContent("Mentions a bad agent");
    expect(useCommentDraftStore.getState().getAnnotations(draftKey)).toHaveLength(0);
  });

  it("keeps the unsent note when the send is rejected and clears it on a successful send", async () => {
    const onSubmit = vi.fn(async () => false);
    const view = renderWithI18n(<Fixture onSubmit={onSubmit} />);
    selectText(view.container, "child");
    fireEvent.click(await screen.findByRole("button", { name: "Add to reply" }));
    await typeNote("Rejected once");
    fireEvent.click(screen.getByRole("button", { name: "Reply" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalled());
    fireEvent.pointerDown(document.body);
    await waitFor(() => expect(screen.queryByRole("textbox", { name: "Comment" })).not.toBeInTheDocument());
    selectText(view.container, "child");
    fireEvent.click(await screen.findByRole("button", { name: "Add to reply" }));
    expect(await screen.findByRole("textbox", { name: "Comment" })).toHaveTextContent("Rejected once");

    onSubmit.mockResolvedValueOnce(true);
    fireEvent.click(screen.getByRole("button", { name: "Reply" }));
    await waitFor(() => expect(screen.queryByRole("textbox", { name: "Comment" })).not.toBeInTheDocument());
    selectText(view.container, "child");
    fireEvent.click(await screen.findByRole("button", { name: "Add to reply" }));
    expect(await screen.findByRole("textbox", { name: "Comment" })).not.toHaveTextContent("Rejected once");
  });

  it.each([false, true])("copies the deleted target's latest note before leaving (quota full: %s)", async (full) => {
    if (full) {
      for (let i = 0; i < 20; i++) useCommentDraftStore.getState().addAnnotation(draftKey, {
        id: `saved-${i}`, sourceCommentId: "root", sourceActorName: "Root",
        quote: "Root", start: i, prefix: "", suffix: " text", note: `Saved ${i}`,
      });
    }
    const view = renderWithI18n(<Fixture />);
    selectText(view.container, "child");
    fireEvent.click(await screen.findByRole("button", { name: "Add to reply" }));
    const input = await typeNote("Do not lose this instruction");
    view.rerender(<Fixture deleted />);
    expect(await screen.findByRole("alert")).toHaveTextContent("Copy your note before selecting another comment");
    expect(screen.getByRole("button", { name: "Reply" })).toBeDisabled();
    if (full) expect(screen.getByRole("button", { name: "Add annotation" })).toBeDisabled();
    // Copy reads the editor synchronously, including edits not yet debounced.
    await act(async () => { (input as HTMLElement & { editor: Editor }).editor.commands.setContent("Latest instruction"); });
    const copy = screen.getByRole("button", { name: "Copy note" });
    fireEvent.pointerDown(copy);
    fireEvent.click(copy);
    await waitFor(() => expect(writeText).toHaveBeenCalledWith("Latest instruction"));
    expect(input).toBeInTheDocument();
    selectText(view.container, "root");
    fireEvent.click(await screen.findByRole("button", { name: "Add to reply" }));
    await act(async () => { (screen.getByRole("textbox", { name: "Comment" }) as HTMLElement & { editor: Editor }).editor.commands.setContent(writeText.mock.calls[0]![0]); });
    expect(screen.getByRole("textbox", { name: "Comment" })).toHaveTextContent("Latest instruction");
  });

  it("keeps the note and shows manual recovery when clipboard copying fails", async () => {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: undefined });
    Object.defineProperty(document, "execCommand", { configurable: true, value: vi.fn(() => false) });
    const error = vi.spyOn(toast, "error");
    const view = renderWithI18n(<Fixture />);
    selectText(view.container, "child");
    fireEvent.click(await screen.findByRole("button", { name: "Add to reply" }));
    const input = await typeNote("Recover manually");
    view.rerender(<Fixture deleted />);
    const copy = await screen.findByRole("button", { name: "Copy note" });
    fireEvent.pointerDown(copy);
    copy.focus();
    fireEvent.click(copy);
    await waitFor(() => expect(error).toHaveBeenCalledWith("Could not copy note. Select and copy the text manually."));
    expect(input).toBeInTheDocument();
    expect(input).toHaveTextContent("Recover manually");
  });

  it.each([false, true])("keeps the unsent note when copying its hovered link (clipboard fallback: %s)", async (fallback) => {
    if (fallback) {
      Object.defineProperty(navigator, "clipboard", { configurable: true, value: undefined });
      Object.defineProperty(document, "execCommand", { configurable: true, value: vi.fn(() => true) });
    }
    const view = renderWithI18n(<Fixture />);
    selectText(view.container, "child");
    fireEvent.click(await screen.findByRole("button", { name: "Add to reply" }));
    const input = await typeNote('<p>Please inspect <a href="https://example.com">this page</a></p>');
    const link = input.querySelector("a")!;
    fireEvent.mouseOver(link);
    const copy = await screen.findByRole("button", { name: "Copy link" });
    fireEvent.pointerDown(copy);
    copy.focus();
    fireEvent.click(copy);
    await waitFor(() => expect(fallback ? document.execCommand : writeText).toHaveBeenCalledWith(fallback ? "copy" : "https://example.com"));
    expect(input).toBeInTheDocument();
    expect(input).toHaveTextContent("Please inspect this page");
  });
});
