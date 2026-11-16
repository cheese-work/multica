import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, screen, waitFor } from "@testing-library/react";
import { QueryClientProvider } from "@tanstack/react-query";
import { api, ApiError } from "@multica/core/api";
import { createQueryClient } from "@multica/core/query-client";
import type { Issue } from "@multica/core/types";
import { renderWithI18n } from "../test/i18n";
import { NavigationProvider } from "../navigation/context";

vi.mock("../issues/components/issue-chip", () => ({
  IssueChip: ({ issueId }: { issueId: string }) => (
    <span data-testid="issue-chip">{issueId}</span>
  ),
}));

vi.mock("../issues/components/issue-hover-card", () => ({
  IssueHoverCard: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

vi.mock("../editor/link-hover-card", () => ({
  useLinkHover: () => ({}),
  LinkHoverCard: () => null,
}));

vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: { getIssue: vi.fn(), getAttachmentTextContent: vi.fn() },
}));

vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/paths")>()),
  useCurrentWorkspace: () => ({ id: "workspace-1", issue_prefix: "MUL" }),
  useWorkspacePaths: () => ({
    issueDetail: (id: string) => `/acme/issues/${id}`,
    projectDetail: (id: string) => `/acme/projects/${id}`,
  }),
  useWorkspaceSlug: () => "acme",
}));

vi.mock("mermaid", () => ({
  default: { initialize: vi.fn(), render: vi.fn() },
}));

import { RichContent } from "./rich-content";

const navigation = {
  push: vi.fn(),
  replace: vi.fn(),
  back: vi.fn(),
  pathname: "/",
  searchParams: new URLSearchParams(),
  hash: "",
  getShareableUrl: (path: string) => `https://app.example${path}`,
};

const issue = { id: "uuid-MUL-7", identifier: "MUL-7" } as unknown as Issue;

describe("RichContent bare-key loading box", () => {
  let queryClient: ReturnType<typeof createQueryClient>;
  let settle: (outcome: Issue | ApiError) => void;

  beforeEach(() => {
    queryClient = createQueryClient();
    queryClient.setDefaultOptions({
      queries: { ...queryClient.getDefaultOptions().queries, retry: false },
    });
    vi.mocked(api.getIssue).mockReset().mockImplementation(
      () =>
        new Promise((done, fail) => {
          settle = (outcome) =>
            outcome instanceof Error ? fail(outcome) : done(outcome);
        }),
    );
  });

  afterEach(() => queryClient.clear());

  function renderKey() {
    return renderWithI18n(
      <QueryClientProvider client={queryClient}>
        <NavigationProvider value={navigation}>
          <RichContent content="see MUL-7 now" phase="settled" />
        </NavigationProvider>
      </QueryClientProvider>,
    );
  }

  it("reserves a non-navigable box that keeps the key text while the lookup is pending", async () => {
    const { container } = renderKey();
    await waitFor(() => expect(api.getIssue).toHaveBeenCalledTimes(1));

    const box = container.querySelector("[data-issue-chip-loading]");
    expect(box).not.toBeNull();
    expect(box?.textContent).toBe("MUL-7");
    expect(container.querySelector("a")).toBeNull();
    expect(screen.queryByTestId("issue-chip")).toBeNull();
  });

  it("replaces the box with the chip when the lookup resolves", async () => {
    const { container } = renderKey();
    await waitFor(() => expect(api.getIssue).toHaveBeenCalledTimes(1));

    await act(async () => settle(issue));

    await screen.findByTestId("issue-chip");
    expect(container.querySelector("[data-issue-chip-loading]")).toBeNull();
  });

  it("falls back to the plain key text when the lookup 404s", async () => {
    const { container } = renderKey();
    await waitFor(() => expect(api.getIssue).toHaveBeenCalledTimes(1));

    await act(async () => settle(new ApiError("Not found", 404, "Not Found")));

    await waitFor(() =>
      expect(container.querySelector("[data-issue-chip-loading]")).toBeNull(),
    );
    expect(screen.queryByTestId("issue-chip")).toBeNull();
    expect(container.textContent).toContain("see MUL-7 now");
    expect(container.querySelector("a")).toBeNull();
  });

  // jsdom has no layout: this checks the label's classes, not painted geometry.
  it("lets a long zero-padded alias shrink and truncate inside the capped box", async () => {
    const alias = `MUL-${"0".repeat(60)}7`;
    const { container } = renderWithI18n(
      <QueryClientProvider client={queryClient}>
        <NavigationProvider value={navigation}>
          <RichContent content={`see ${alias} now`} phase="settled" />
        </NavigationProvider>
      </QueryClientProvider>,
    );
    await waitFor(() => expect(api.getIssue).toHaveBeenCalledTimes(1));

    const label = container.querySelector("[data-issue-chip-loading] > span:last-child")!;

    expect(label.textContent).toBe(alias);
    expect(label.classList).toContain("min-w-0");
    expect(label.classList).toContain("truncate");
    expect(label.classList).not.toContain("shrink-0");
  });
});
