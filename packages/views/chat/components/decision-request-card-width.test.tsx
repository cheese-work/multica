import { screen } from "@testing-library/react";
import { forwardRef } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderWithI18n } from "../../test/i18n";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
import { DecisionRequestCard } from "./decision-request-card";

vi.mock("../../platform", () => ({ openExternal: vi.fn() }));
vi.mock("../../platform/local-directory", () => ({ isDesktopShell: () => false }));
vi.mock("../../navigation", async (importActual) => ({
  ...(await importActual<typeof import("../../navigation")>()),
  useOptionalNavigation: () => null,
  AppLink: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => <a href={href} {...rest}>{children}</a>,
}));
vi.mock("../../issues/components/issue-chip", () => ({
  IssueChip: ({ fallbackLabel, children }: { fallbackLabel?: string; children?: React.ReactNode }) => <span>{children ?? fallbackLabel}</span>,
}));
vi.mock("../../issues/components/comment-mention-card", () => ({
  CommentMentionCard: ({ fallback }: { fallback: React.ReactNode }) => <>{fallback}</>,
}));
const listDecisionArtifacts = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/api", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/api")>()),
  api: { listDecisionArtifacts },
}));
vi.mock("./lazy-content-editor", () => ({
  LazyContentEditor: forwardRef(function MockEditor() {
    return <textarea aria-label="note" />;
  }),
}));

const card: RequestCard = {
  id: "request", issue_id: "issue", issue_identifier: "CHE-1", issue_title: "Sign in",
  ask: "Will you sign in?", state: "open", created_at: "2026-11-07T07:02:00Z",
  options: [{ label: "Yes", what_happens: "x".repeat(300) }],
};

// jsdom has no layout, so assert the classes that keep the fold grid's track and its child
// shrinkable: a bare grid column and a child without min-w-0 size to the longest line and
// push the card content past the section border (CHE-5459).
describe("DecisionRequestCard width", () => {
  beforeEach(() => listDecisionArtifacts.mockResolvedValue([]));

  it("keeps the fold grid track and its child shrinkable", () => {
    renderWithI18n(
      <QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={card} inIssue /></WorkspaceSlugProvider></QueryClientProvider>,
    );
    const grid = screen.getByTestId("decision-request-card").querySelector("[data-fold-body]")!;
    expect(grid.className).toContain("grid-cols-[minmax(0,1fr)]");
    expect(grid.firstElementChild).toHaveClass("min-w-0");
  });
});
