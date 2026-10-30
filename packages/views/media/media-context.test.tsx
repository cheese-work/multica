import { webcrypto } from "node:crypto";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { WorkspaceSlugProvider, type MediaKind } from "@multica/core/paths";
import { NavigationProvider, type NavigationAdapter } from "../navigation";
import { renderWithI18n } from "../test/i18n";
import { MediaLinkAction, MediaSourceProvider, StandaloneMediaProvider, ViewerActions } from "./media-context";
import { mediaContentId } from "./media-id";

const { copy, toastError } = vi.hoisted(() => ({ copy: vi.fn(), toastError: vi.fn() }));
vi.mock("@multica/ui/lib/clipboard", () => ({ copyText: copy }));
vi.mock("sonner", () => ({ toast: { error: toastError } }));

const chart = "graph TD\nA --> B";
function renderAction(overrides: Partial<NavigationAdapter> = {}, standalone = false, kind: MediaKind = "mermaid") {
  const navigation: NavigationAdapter = { push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/media/issues/issue-1/mermaid/hash", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (path) => `https://multica.example.test${path}`, ...overrides };
  return renderWithI18n(<WorkspaceSlugProvider slug="acme"><NavigationProvider value={navigation}><MediaSourceProvider source={{ type: "issues", id: "issue-1" }}><StandaloneMediaProvider value={standalone}><MediaLinkAction kind={kind} content={chart} /></StandaloneMediaProvider></MediaSourceProvider></NavigationProvider></WorkspaceSlugProvider>);
}

beforeEach(() => { vi.clearAllMocks(); vi.stubGlobal("crypto", webcrypto); copy.mockResolvedValue(true); });

describe("media viewer actions", () => {
  it("opens a source-scoped browser tab without putting media bytes in the URL", async () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    renderAction();
    const button = screen.getByRole("button", { name: "Open in new tab" });
    await waitFor(() => expect(button).toBeEnabled());
    fireEvent.click(button);
    expect(open).toHaveBeenCalledWith(`https://multica.example.test/acme/media/issues/issue-1/mermaid/${await mediaContentId(chart)}`, "_blank", "noopener,noreferrer");
  });

  it.each<[MediaKind, string]>([["mermaid", "Mermaid diagram"], ["table", "Table"], ["html", "HTML preview"], ["image", "View image"], ["attachment", "Media"]])("uses a native app window with a readable title for %s", async (kind, title) => {
    const openMediaWindow = vi.fn();
    const openInNewTab = vi.fn();
    renderAction({ openMediaWindow, openInNewTab }, false, kind);
    const button = screen.getByRole("button", { name: "Open in new window" });
    await waitFor(() => expect(button).toBeEnabled());
    fireEvent.click(button);
    expect(openMediaWindow).toHaveBeenCalledWith(`/acme/media/issues/issue-1/${kind}/${await mediaContentId(chart)}`, title);
    expect(openInNewTab).not.toHaveBeenCalled();
  });

  it("copies the public media URL, including the current query and fragment", async () => {
    renderAction({ searchParams: new URLSearchParams("view=source"), hash: "#item" }, true);
    fireEvent.click(screen.getByRole("button", { name: "Copy link" }));
    await waitFor(() => expect(copy).toHaveBeenCalledWith("https://multica.example.test/acme/media/issues/issue-1/mermaid/hash?view=source#item"));
    expect(await screen.findByRole("button", { name: "Link copied" })).toBeTruthy();
  });

  it("keeps phone More actions as focusable buttons and returns focus on Escape", async () => {
    const oldWidth = window.innerWidth;
    Object.defineProperty(window, "innerWidth", { configurable: true, value: 390 });
    const view = renderWithI18n(<ViewerActions><button type="button">Download</button><button type="button">Copy link</button></ViewerActions>);
    const summary = await screen.findByLabelText("More");
    expect(summary.tagName).toBe("SUMMARY");
    fireEvent.click(summary);
    const button = screen.getByRole("button", { name: "Download" });
    button.focus();
    expect(button).toHaveFocus();
    fireEvent.keyDown(button, { key: "Escape" });
    expect(summary).toHaveFocus();
    expect(summary.closest("details")).not.toHaveAttribute("open");
    view.unmount();
    Object.defineProperty(window, "innerWidth", { configurable: true, value: oldWidth });
  });

  it("lets Escape on the closed More summary reach the document without preventing default", async () => {
    const oldWidth = window.innerWidth;
    Object.defineProperty(window, "innerWidth", { configurable: true, value: 390 });
    const listener = vi.fn();
    document.addEventListener("keydown", listener);
    const view = renderWithI18n(<ViewerActions><button type="button">Download</button></ViewerActions>);
    try {
      const summary = await screen.findByLabelText("More");
      expect(summary.closest("details")).not.toHaveAttribute("open");
      summary.focus();
      const event = new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true });
      fireEvent(summary, event);
      expect(listener).toHaveBeenCalledOnce();
      expect(event.defaultPrevented).toBe(false);
    } finally {
      view.unmount();
      document.removeEventListener("keydown", listener);
      Object.defineProperty(window, "innerWidth", { configurable: true, value: oldWidth });
    }
  });

  it("reports native-window failures instead of silently falling back to an external browser", async () => {
    renderAction({ openMediaWindow: vi.fn().mockRejectedValue(new Error("Rejected")) });
    const button = screen.getByRole("button", { name: "Open in new window" });
    await waitFor(() => expect(button).toBeEnabled());
    fireEvent.click(button);
    await waitFor(() => expect(toastError).toHaveBeenCalledWith("Couldn't open the media page"));
  });

  it("restores More focus even when a nested action consumes Escape", async () => {
    const oldWidth = window.innerWidth;
    Object.defineProperty(window, "innerWidth", { configurable: true, value: 390 });
    const view = renderWithI18n(<ViewerActions><button type="button" onKeyDown={(event) => event.stopPropagation()}>Show source</button></ViewerActions>);
    const summary = await screen.findByLabelText("More");
    fireEvent.click(summary);
    const button = screen.getByRole("button", { name: "Show source" });
    button.focus();
    fireEvent.keyDown(button, { key: "Escape" });
    expect(summary).toHaveFocus();
    expect(summary.closest("details")).not.toHaveAttribute("open");
    view.unmount();
    Object.defineProperty(window, "innerWidth", { configurable: true, value: oldWidth });
  });
});
