import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { ApiClient, setApiInstance } from "@multica/core/api";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { NavigationProvider } from "../navigation/context";
import { renderWithI18n } from "../test/i18n";
import { MermaidViewer } from "../editor/mermaid-viewer";
import { HtmlPreviewSurface } from "../editor/html-preview-surface";
import { AttachmentPreviewModal } from "../editor/attachment-preview-modal";
import { RichTableDialog } from "../rich-content/rich-table-dialog";
import { mockNativeDialog } from "../editor/test-utils/native-dialog";

describe("phone source-dialog Close", () => {
  const oldWidth = window.innerWidth;
  let restoreDialog: () => void;
  beforeEach(() => {
    setApiInstance(new ApiClient(""));
    Object.defineProperty(window, "innerWidth", { configurable: true, value: 390 });
    restoreDialog = mockNativeDialog();
  });
  afterEach(() => {
    cleanup();
    restoreDialog();
    Object.defineProperty(window, "innerWidth", { configurable: true, value: oldWidth });
  });

  it.each(["mermaid", "table", "image", "html"] as const)("keeps %s Close outside More and usable without opening it", async (kind) => {
    const close = vi.fn();
    if (kind === "mermaid") renderWithI18n(<MermaidViewer open onOpenChange={close} chart="graph LR; A-->B" svg='<svg viewBox="0 0 100 50"></svg>' viewerDocument="<html></html>" layout={{ width: 100, height: 50 }} exportBackground="white" exportFontFamily="sans-serif" />);
    if (kind === "table") renderWithI18n(<RichTableDialog open onOpenChange={close} getMarkdown={() => "| Name |\n| --- |\n| Saved |"} model={{ header: ["Name"], rows: [["Saved"]], headerText: ["Name"], rowText: [["Saved"]] }} />);
    if (kind === "image") renderWithI18n(
      <NavigationProvider value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (path) => path }}>
        <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })}>
          <AttachmentPreviewModal open onClose={close} source={{ kind: "url", url: "https://example.test/saved.png", filename: "saved.png", forceKind: "image" }} />
        </QueryClientProvider>
      </NavigationProvider>,
    );
    if (kind === "html") renderWithI18n(<HtmlPreviewSurface open onOpenChange={close} inlineHeight={200} title="HTML preview"><iframe title="Saved HTML" /></HtmlPreviewSurface>);
    const more = await screen.findByLabelText("More");
    await waitFor(() => expect(more.closest("details")).not.toHaveAttribute("open"));
    const button = screen.getByRole("button", { name: "Close" });
    expect(button.closest("details")).toBeNull();
    button.focus();
    expect(button).toHaveFocus();
    fireEvent.click(button);
    expect(close).toHaveBeenCalled();
  });
});
