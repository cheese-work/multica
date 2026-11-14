import { describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import type { Issue } from "@multica/core/types";
import { renderWithI18n } from "../test/i18n";
import { NavigationProvider } from "../navigation/context";

vi.mock("../issues/hooks", () => ({
  useResolveIssueIdentifier: (identifier: string) =>
    ({ id: `uuid-${identifier}`, identifier }) as unknown as Issue,
}));

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

vi.mock("@multica/core/api", () => ({
  api: { getAttachmentTextContent: vi.fn() },
  PreviewTooLargeError: class extends Error {},
  PreviewUnsupportedError: class extends Error {},
}));

vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/paths")>()),
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

function chipIds(): string[] {
  return screen.queryAllByTestId("issue-chip").map((chip) => chip.textContent ?? "");
}

function renderRich(content: string, phase: "streaming" | "settled") {
  const tree = (c: string, p: "streaming" | "settled") => (
    <NavigationProvider value={navigation}>
      <RichContent content={c} phase={p} />
    </NavigationProvider>
  );
  const view = renderWithI18n(tree(content, phase));
  return {
    update: (c: string, p: "streaming" | "settled") => view.rerender(tree(c, p)),
  };
}

describe("RichContent streaming issue-key autolinks", () => {
  it("renders no chip for any partial key while the key touches the end of a streaming message", () => {
    const { update } = renderRich("see C", "streaming");
    const seen: string[][] = [];
    for (const partial of ["see CHE-", "see CHE-1", "see CHE-18", "see CHE-182", "see CHE-1824"]) {
      update(partial, "streaming");
      seen.push(chipIds());
    }
    expect(seen).toEqual([[], [], [], [], []]);
  });

  it("links a completed key once a boundary character follows it", () => {
    const { update } = renderRich("see CHE-18", "streaming");
    expect(chipIds()).toEqual([]);

    update("see CHE-1824 ", "streaming");
    expect(chipIds()).toEqual(["uuid-CHE-1824"]);

    update("see CHE-1824 and CHE-18", "streaming");
    expect(chipIds()).toEqual(["uuid-CHE-1824"]);
  });

  it("links an end-of-message key when the phase changes to settled", () => {
    const { update } = renderRich("see CHE-1824", "streaming");
    expect(chipIds()).toEqual([]);

    update("see CHE-1824", "settled");
    expect(chipIds()).toEqual(["uuid-CHE-1824"]);
  });

  it("links an end-of-message key immediately when the message is already settled", () => {
    renderRich("see CHE-1824", "settled");
    expect(chipIds()).toEqual(["uuid-CHE-1824"]);
  });
});
