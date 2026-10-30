import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderWithI18n } from "../test/i18n";
import { MediaPage } from "./media-page";
import { NavigationProvider, type NavigationAdapter } from "../navigation";
import type { MediaRoute } from "@multica/core/paths";

const { resolve } = vi.hoisted(() => ({ resolve: vi.fn() }));
vi.mock("./resolve-media", () => ({ resolveMedia: resolve }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/auth", () => ({ useAuthStore: (select: (state: unknown) => unknown) => select({ user: { id: "user-1" } }) }));
vi.mock("../editor/mermaid-diagram", () => ({ MermaidDiagram: ({ chart }: { chart: string }) => <iframe title="private diagram" srcDoc={chart} /> }));
vi.mock("../editor/html-block-preview", () => ({ HtmlBlockPreview: ({ html }: { html: string }) => <iframe title="private HTML" srcDoc={html} /> }));
vi.mock("../editor/attachment-preview-modal", () => ({ AttachmentSourceStandalone: () => <img alt="private media" src="https://example.test/private.png" /> }));
vi.mock("../rich-content", () => ({ RichContent: ({ content }: { content: string }) => <table><tbody><tr><td>{content}</td></tr></tbody></table> }));

const route: MediaRoute = { workspaceSlug: "acme", type: "issues", id: "issue-1", kind: "html", mediaId: "html-1" };
const key = ["media-page", "ws-1", "user-1", "issues", "issue-1", "html", "html-1"];

function renderPage(client = new QueryClient({ defaultOptions: { queries: { retry: false } } })) {
  const navigation: NavigationAdapter = { push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/media/issues/issue-1/html/html-1", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (path) => path };
  renderWithI18n(<NavigationProvider value={navigation}><QueryClientProvider client={client}><MediaPage route={route} /></QueryClientProvider></NavigationProvider>);
  return client;
}

beforeEach(() => { vi.resetAllMocks(); });

describe("media route permission", () => {
  it("matches the normal not-found page with a 404, description and recovery link", async () => {
    resolve.mockRejectedValue(Object.assign(new Error("Not found"), { status: 403 }));
    renderPage();
    expect(await screen.findByRole("heading", { name: "Page not found" })).toBeInTheDocument();
    expect(screen.getByText("404")).toBeInTheDocument();
    expect(screen.getByText("The page you are looking for doesn't exist or has been moved.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Back to Multica" })).toHaveAttribute("href", "/");
    expect(document.querySelector("iframe, img, table")).toBeNull();
  });

  it.each([401, 403, 404])("no access (%s) means no media bytes, even with a cached source", async (status) => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(key, { kind: "html", content: "<h1>secret media bytes</h1>" });
    resolve.mockRejectedValue(Object.assign(new Error("Not found"), { status }));
    renderPage(client);
    expect(document.querySelector("iframe, img, table")).toBeNull();
    expect(await screen.findByText("Page not found")).toBeTruthy();
    expect(document.querySelector("iframe, img, table")).toBeNull();
    expect(document.body.textContent).not.toContain("secret media bytes");
  });

  it("removes media after a later permission revocation", async () => {
    resolve.mockResolvedValue({ kind: "html", content: "private HTML" });
    const client = renderPage();
    expect(await screen.findByTitle("private HTML")).toBeTruthy();
    resolve.mockRejectedValue(Object.assign(new Error("Not found"), { status: 403 }));
    await client.invalidateQueries({ queryKey: key });
    expect(await screen.findByText("Page not found")).toBeTruthy();
    expect(document.querySelector("iframe, img, table")).toBeNull();
  });

  it("retains this view's last loaded copy on an offline error, not cached data on a fresh denied route", async () => {
    resolve.mockResolvedValue({ kind: "html", content: "private HTML" });
    const client = renderPage();
    await screen.findByTitle("private HTML");
    resolve.mockRejectedValue(new TypeError("Failed to fetch"));
    await client.invalidateQueries({ queryKey: key });
    expect(await screen.findByRole("status")).toHaveTextContent("Offline");
    expect(screen.getByTitle("private HTML")).toBeTruthy();
  });

  it("offers retry after a load error without exposing media", async () => {
    resolve.mockRejectedValue(new TypeError("Failed to fetch"));
    renderPage();
    expect(await screen.findByRole("button", { name: "Retry" })).toBeTruthy();
    await waitFor(() => expect(document.querySelector("iframe, img, table")).toBeNull());
  });
});
