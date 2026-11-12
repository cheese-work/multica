import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import type { ReactElement } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

const { getDiffMock } = vi.hoisted(() => ({ getDiffMock: vi.fn() }));

vi.mock("@multica/core/api", () => ({
  api: {
    getAttachmentRevisionDiff: getDiffMock,
    getAttachmentTextContent: vi.fn(),
    getAttachment: vi.fn(),
  },
  PreviewTooLargeError: class extends Error {},
  PreviewUnsupportedError: class extends Error {},
}));

vi.mock("../../navigation", () => ({
  useNavigation: () => ({
    push: vi.fn(),
    replace: vi.fn(),
    back: vi.fn(),
    pathname: "/acme/issues",
    searchParams: new URLSearchParams(),
    hash: "",
    openInNewTab: vi.fn(),
    getShareableUrl: (p: string) => `https://app.example${p}`,
  }),
}));

vi.mock("@multica/core/paths", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@multica/core/paths")>();
  return { ...actual, useWorkspaceSlug: () => "acme" };
});

import { renderWithI18n } from "../../test/i18n";
import { AttachmentList } from "./attachment-list";

const rev = (id: string, revision: number, revisionOf: string | null, filename = "plan.md") =>
  ({
    id,
    url: `/uploads/${id}`,
    download_url: `/uploads/${id}`,
    filename,
    content_type: "text/markdown",
    size_bytes: 2048,
    created_at: "2026-09-20T10:00:00Z",
    revision,
    revision_of: revisionOf,
  }) as any;

function renderList(ui: ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return renderWithI18n(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

const chain = [rev("a1", 1, null), rev("a2", 2, "a1"), rev("a3", 3, "a2")];

beforeEach(() => {
  vi.clearAllMocks();
  getDiffMock.mockResolvedValue({
    from: 2,
    to: 3,
    binary: false,
    diff: "--- plan.md\n+++ plan.md\n@@ -1,2 +1,2 @@\n alpha\n-beta\n+gamma\n",
    message: "",
  });
});
afterEach(cleanup);

describe("AttachmentList — revision chain", () => {
  it("shows one card per document with its revision and earlier count", () => {
    renderList(<AttachmentList attachments={chain} content="" />);
    expect(screen.getAllByRole("button", { name: "plan.md" })).toHaveLength(1);
    expect(screen.getByText("v3 · 2 earlier")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Diff" })).toBeTruthy();
  });

  it("a first revision stays a plain card with no Diff", () => {
    renderList(<AttachmentList attachments={[rev("b1", 1, null, "notes.md")]} content="" />);
    expect(screen.getByRole("button", { name: "notes.md" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Diff" })).toBeNull();
    expect(screen.queryByText(/earlier/)).toBeNull();
  });

  it("Diff opens the previous-to-latest changes, removed and added lines", async () => {
    renderList(<AttachmentList attachments={chain} content="" />);
    fireEvent.click(screen.getByRole("button", { name: "Diff" }));
    await waitFor(() => expect(getDiffMock).toHaveBeenCalledWith("a3", 2, 3, false));
    expect(await screen.findByText("beta")).toBeTruthy();
    expect(screen.getByText("gamma")).toBeTruthy();
  });

  it("Show full refetches with every unchanged line", async () => {
    renderList(<AttachmentList attachments={chain} content="" />);
    fireEvent.click(screen.getByRole("button", { name: "Diff" }));
    fireEvent.click(await screen.findByRole("button", { name: "Show full" }));
    await waitFor(() => expect(getDiffMock).toHaveBeenCalledWith("a3", 2, 3, true));
    expect(await screen.findByRole("button", { name: "Show changes only" })).toBeTruthy();
  });

  it("a binary pair shows the server message instead of lines", async () => {
    getDiffMock.mockResolvedValue({
      from: 1,
      to: 2,
      binary: true,
      diff: "",
      message: "binary, sizes and SHA differ",
    });
    renderList(
      <AttachmentList attachments={[rev("p1", 1, null, "pic.md"), rev("p2", 2, "p1", "pic.md")]} content="" />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Diff" }));
    expect(await screen.findByText("binary, sizes and SHA differ")).toBeTruthy();
  });

  it("a failed diff says so", async () => {
    getDiffMock.mockRejectedValue(new Error("boom"));
    renderList(<AttachmentList attachments={chain} content="" />);
    fireEvent.click(screen.getByRole("button", { name: "Diff" }));
    expect(await screen.findByText("Could not load the changes.")).toBeTruthy();
  });
});
