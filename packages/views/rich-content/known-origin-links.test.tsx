/**
 * Multica links on any known host render in-app on the current origin (CHE-4832).
 *
 * Cheese opens Multica at multica.congvc.dev, but agents paste Tailscale-host
 * links (decision `card_link`, DM replies). A link whose origin is a known
 * Multica origin AND whose path is a Multica route becomes a chip or is
 * rewritten to the current origin; everything else is left alone.
 */
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { NavigationProvider } from "../navigation/context";
import type { NavigationAdapter } from "../navigation/types";

const getChatSession = vi.fn();
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
    getChatSession: (id: string) => getChatSession(id),
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
    chat: () => `/acme/chat`,
  }),
  useWorkspaceSlug: () => "acme",
}));

vi.mock("../issues/components/issue-mention-card", () => ({
  IssueMentionCard: ({ issueId }: { issueId: string }) => (
    <span data-testid="issue-chip">{issueId}</span>
  ),
}));

vi.mock("../editor/link-hover-card", () => ({
  useLinkHover: () => ({}),
  LinkHoverCard: () => null,
}));

import { RichContent } from "./rich-content";

const CURRENT = "https://multica.congvc.dev";
const TAILSCALE = "https://congvc-c00.taila6fa8a.ts.net:19443";
const SESSION = "01a1bb4b-8f5d-72ed-81a4-f1af9eff03e9";
const MESSAGE = "01a1bf7d-d97b-7482-816d-e8462193b43c";
const ISSUE_ID = "1b9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed";
const COMMENT_ID = "01a1769e-060e-7b2c-84dd-b396f8d9a660";

function renderContent(content: string, origin = CURRENT) {
  const adapter: NavigationAdapter = {
    push: vi.fn(),
    replace: vi.fn(),
    back: vi.fn(),
    pathname: "/",
    searchParams: new URLSearchParams(),
    hash: "",
    getShareableUrl: (p) => `${origin}${p}`,
  };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <NavigationProvider value={adapter}>
        <RichContent content={content} />
      </NavigationProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  getChatSession.mockReset();
  getCommentPreview.mockReset();
  getChatSession.mockResolvedValue({ id: SESSION, title: "Fix the links" });
});

describe("known-origin Multica links", () => {
  it.each([
    ["tailscale host", TAILSCALE],
    ["multica.congvc.dev", CURRENT],
  ])("renders a %s DM link as a DM chip", async (_n, host) => {
    const { container } = renderContent(`See ${host}/acme/chat?session=${SESSION}`);

    expect(await screen.findByText("Fix the links")).toBeTruthy();
    expect(
      container.querySelector(`a[href="/acme/chat?session=${SESSION}"]`),
    ).not.toBeNull();
  });

  it("keeps &message= on the DM chip link", async () => {
    const { container } = renderContent(
      `${TAILSCALE}/acme/chat?session=${SESSION}&message=${MESSAGE}`,
    );

    expect(await screen.findByText("Fix the links")).toBeTruthy();
    expect(container.querySelector("a")?.getAttribute("href")).toBe(
      `/acme/chat?session=${SESSION}&message=${MESSAGE}`,
    );
  });

  it("renders a tailscale issue URL as an issue chip", () => {
    renderContent(`See ${TAILSCALE}/acme/issues/${ISSUE_ID} now`);
    expect(screen.getByTestId("issue-chip").textContent).toBe(ISSUE_ID);
  });

  it("renders a tailscale issue URL with #comment- as a comment chip", async () => {
    getCommentPreview.mockResolvedValue({
      comment_id: COMMENT_ID,
      issue_id: ISSUE_ID,
      issue_identifier: "CHE-1",
      author_type: "agent",
      author_id: "a1",
      author_name: "Luna",
      preview: "first line",
    });
    const { container } = renderContent(
      `${TAILSCALE}/acme/issues/${ISSUE_ID}#comment-${COMMENT_ID}`,
    );

    await waitFor(() => expect(screen.getByText(/first line/)).toBeTruthy());
    expect(
      container.querySelector(`a[href="/acme/issues/${ISSUE_ID}#comment-${COMMENT_ID}"]`),
    ).not.toBeNull();
  });

  it("rewrites another Multica-route link to the current origin", () => {
    const { container } = renderContent(`[agent](${TAILSCALE}/acme/agents/${ISSUE_ID}?tab=x)`);
    expect(
      container.querySelector(`a[href="${CURRENT}/acme/agents/${ISSUE_ID}?tab=x"]`),
    ).not.toBeNull();
  });

  it("rewrites to the current origin when it is neither known host", () => {
    const { container } = renderContent(
      `[agent](${TAILSCALE}/acme/agents/${ISSUE_ID})`,
      "http://localhost:3000",
    );
    expect(
      container.querySelector(`a[href="http://localhost:3000/acme/agents/${ISSUE_ID}"]`),
    ).not.toBeNull();
  });

  it("leaves an external link unchanged", () => {
    const { container } = renderContent(`[pr](https://github.com/acme/chat?session=${SESSION})`);
    expect(
      container.querySelector(`a[href="https://github.com/acme/chat?session=${SESSION}"]`),
    ).not.toBeNull();
  });

  it("leaves a lookalike host unchanged", () => {
    const url = `https://multica.congvc.dev.evil.com/acme/chat?session=${SESSION}`;
    const { container } = renderContent(url);
    expect(container.querySelector(`a[href="${url}"]`)).not.toBeNull();
    expect(getChatSession).not.toHaveBeenCalled();
  });

  it("leaves a known host non-route path unchanged", () => {
    const { container } = renderContent(`[x](${TAILSCALE}/uploads/a.pdf)`);
    expect(container.querySelector(`a[href="${TAILSCALE}/uploads/a.pdf"]`)).not.toBeNull();
  });
});
