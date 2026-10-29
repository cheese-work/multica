/**
 * Comment IDs render as chips (CHE-2120).
 *
 * A full comment UUID, or a bare in-app issue URL carrying `#comment-<uuid>`,
 * becomes `CHE-1 · <author>: <first line>` linking to the comment. Anything the
 * server will not resolve — unknown, other workspace, no read access — and any
 * short prefix stay plain text.
 */
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { NavigationProvider } from "../navigation/context";
import type { NavigationAdapter } from "../navigation/types";

const getCommentPreview = vi.fn();

vi.mock("../i18n", async () => {
  const editor = (await import("../locales/en/editor.json")).default;
  return {
    useT: () => ({
      t: (select: (bundle: typeof editor) => string) => select(editor),
    }),
    useTimeAgo: () => "just now",
  };
});

vi.mock("@multica/core/api", () => ({
  api: {
    getAttachmentTextContent: vi.fn(),
    getCommentPreview: (id: string) => getCommentPreview(id),
  },
  PreviewTooLargeError: class extends Error {},
  PreviewUnsupportedError: class extends Error {},
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));

vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/paths")>()),
  useWorkspacePaths: () => ({
    issueDetail: (id: string) => `/acme/issues/${id}`,
    projectDetail: (id: string) => `/acme/projects/${id}`,
  }),
  useWorkspaceSlug: () => "acme",
}));

vi.mock("../editor/link-hover-card", () => ({
  useLinkHover: () => ({}),
  LinkHoverCard: () => null,
}));

import { RichContent } from "./rich-content";

const APP_ORIGIN = "https://app.multica.ai";
const COMMENT_ID = "01a1769e-060e-7b2c-84dd-b396f8d9a660";
const ISSUE_ID = "1b9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed";

const PREVIEW = {
  comment_id: COMMENT_ID,
  issue_id: ISSUE_ID,
  issue_identifier: "CHE-1",
  author_type: "agent",
  author_id: "a1",
  author_name: "Luna",
  preview: "first line of the comment",
};

function adapter(): NavigationAdapter {
  return {
    push: vi.fn(),
    replace: vi.fn(),
    back: vi.fn(),
    pathname: "/",
    searchParams: new URLSearchParams(),
    hash: "",
    getShareableUrl: (p) => `${APP_ORIGIN}${p}`,
  };
}

function renderContent(content: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <NavigationProvider value={adapter()}>
        <RichContent content={content} />
      </NavigationProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  getCommentPreview.mockReset();
});

describe("comment ID chips", () => {
  it("renders a full comment UUID as a chip with issue key, author and preview", async () => {
    getCommentPreview.mockResolvedValue(PREVIEW);
    const { container } = renderContent(`Look at ${COMMENT_ID} please`);

    const link = await waitFor(() => {
      const a = container.querySelector(`a[href="/acme/issues/${ISSUE_ID}#comment-${COMMENT_ID}"]`);
      expect(a).not.toBeNull();
      return a as HTMLElement;
    });
    expect(link.textContent).toContain("CHE-1");
    expect(link.textContent).toContain("Luna");
    expect(link.textContent).toContain("first line of the comment");
    expect(getCommentPreview).toHaveBeenCalledWith(COMMENT_ID);
  });

  it("renders a bare issue URL with #comment-<uuid> as the same chip", async () => {
    getCommentPreview.mockResolvedValue(PREVIEW);
    const { container } = renderContent(
      `See ${APP_ORIGIN}/acme/issues/CHE-1#comment-${COMMENT_ID} now`,
    );

    await waitFor(() => {
      expect(
        container.querySelector(`a[href="/acme/issues/${ISSUE_ID}#comment-${COMMENT_ID}"]`),
      ).not.toBeNull();
    });
  });

  it("keeps an unresolvable UUID as plain text", async () => {
    getCommentPreview.mockRejectedValue(new Error("404"));
    const { container } = renderContent(`Look at ${COMMENT_ID} please`);

    await waitFor(() => expect(getCommentPreview).toHaveBeenCalled());
    expect(container.querySelector("a")).toBeNull();
    expect(container.textContent).toContain(COMMENT_ID);
  });

  it("leaves a short prefix as plain text and never asks the server", () => {
    renderContent("Look at 01a1769e-060e please");

    expect(screen.getByText(/01a1769e-060e/)).toBeTruthy();
    expect(getCommentPreview).not.toHaveBeenCalled();
  });
});
