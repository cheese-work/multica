import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { createRef } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { renderWithI18n } from "../test/i18n";
import { NavigationProvider } from "../navigation/context";
import { MediaSourceProvider } from "../media/media-context";
import { ContentEditor, type ContentEditorRef } from "./content-editor";
import { mockNativeDialog } from "./test-utils/native-dialog";

vi.mock("./link-hover-card", () => ({
  useLinkHover: () => ({}),
  LinkHoverCard: () => null,
}));

const SAVED = "```html\n<p>Saved description</p>\n```";
const DRAFT = "```html\n<p>Unsaved draft</p>\n```";

function renderEditor(persistedMediaMarkdown?: string, content = SAVED) {
  const ref = createRef<ContentEditorRef>();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  renderWithI18n(
    <WorkspaceSlugProvider slug="acme">
      <NavigationProvider value={{
        push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/issues/issue-1",
        searchParams: new URLSearchParams(), hash: "",
        getShareableUrl: (path) => `https://multica.example.test${path}`,
      }}>
        <QueryClientProvider client={queryClient}>
          <MediaSourceProvider source={{ type: "issues", id: "issue-1" }}>
            <ContentEditor ref={ref} defaultValue={content} persistedMediaMarkdown={persistedMediaMarkdown} showBubbleMenu={false} />
          </MediaSourceProvider>
        </QueryClientProvider>
      </NavigationProvider>
    </WorkspaceSlugProvider>,
  );
  return ref;
}

describe("persisted description media in ContentEditor", () => {
  let restoreDialog: () => void;
  beforeEach(() => { restoreDialog = mockNativeDialog(); });
  afterEach(() => { cleanup(); restoreDialog(); vi.restoreAllMocks(); });

  it("keeps Open in new tab for a saved description block", async () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    renderEditor(SAVED);
    fireEvent.click(await screen.findByRole("button", { name: "Fullscreen" }));
    const action = within(screen.getByRole("dialog")).getByRole("button", { name: "Open in new tab" });
    await waitFor(() => expect(action).toBeEnabled());
    fireEvent.click(action);
    expect(open).toHaveBeenCalledWith(expect.stringMatching(/^https:\/\/multica\.example\.test\/acme\/media\/issues\/issue-1\/html\/[a-f0-9]+$/), "_blank", "noopener,noreferrer");
  });

  it("has no Open action for an unsaved description block", async () => {
    renderEditor(SAVED, DRAFT);
    fireEvent.click(await screen.findByRole("button", { name: "Fullscreen" }));
    expect(within(screen.getByRole("dialog")).queryByTestId("media-link-action")).toBeNull();
  });

  it("hides the action immediately after an edit and restores it after undo", async () => {
    const ref = renderEditor(SAVED);
    fireEvent.click(await screen.findByRole("button", { name: "Fullscreen" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByTestId("media-link-action")).toBeTruthy();
    await act(async () => ref.current?.adoptContent(DRAFT));
    expect(within(dialog).queryByTestId("media-link-action")).toBeNull();
    await act(async () => ref.current?.adoptContent(SAVED));
    await waitFor(() => expect(within(screen.getByRole("dialog")).getByTestId("media-link-action")).toBeEnabled());
  });

  it("has no Open action in a composer even when its text matches saved description media", async () => {
    renderEditor();
    fireEvent.click(await screen.findByRole("button", { name: "Fullscreen" }));
    expect(within(screen.getByRole("dialog")).queryByTestId("media-link-action")).toBeNull();
  });
});
