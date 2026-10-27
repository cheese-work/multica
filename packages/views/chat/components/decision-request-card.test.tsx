import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderWithI18n } from "../../test/i18n";
import { DecisionRequestCard } from "./decision-request-card";

vi.mock("../../navigation", async (importActual) => ({
  ...(await importActual<typeof import("../../navigation")>()),
  AppLink: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => <a href={href} {...rest}>{children}</a>,
}));

vi.mock("../../issues/components/issue-chip", () => ({
  IssueChip: ({ fallbackLabel, children }: { fallbackLabel?: string; children?: React.ReactNode }) => <span data-testid="issue-chip">{children ?? fallbackLabel}</span>,
}));

const card = {
  id: "request", issue_id: "issue", issue_identifier: "CHE-2088",
  source_comment_id: "source", ask: "Keep the **original ask**.",
  state: "open" as const, created_at: "2026-10-26T23:30:00Z",
};

describe("DecisionRequestCard accessibility", () => {
  it("exposes named keyboard-accessible source links in reading order", async () => {
    renderWithI18n(<QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={card} /></WorkspaceSlugProvider></QueryClientProvider>);
    const user = userEvent.setup();
    await user.tab();
    expect(screen.getByRole("link", { name: "CHE-2088" })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("link", { name: "Source comment" })).toHaveFocus();
    expect(screen.getByRole("region", { name: "Decision request: Open" })).toHaveTextContent("27 Oct 2026, 06:30 ICT");
    expect(document.querySelector("time")).toHaveAttribute("datetime", card.created_at);
  });

  it("keeps a localized status label and tolerates missing legacy source and invalid time", () => {
    renderWithI18n(<QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={{ ...card, state: "withdrawn", source_comment_id: undefined, created_at: "invalid", reason: "Scope changed." }} /></WorkspaceSlugProvider></QueryClientProvider>, { locale: "zh-Hans" });
    expect(screen.getByRole("region", { name: "决策请求：已撤回" })).toHaveTextContent("Scope changed.");
    expect(screen.getByText(/original ask/)).toBeInTheDocument();
    expect(screen.getAllByRole("link")).toHaveLength(1);
    expect(document.querySelector("time")).toBeNull();
  });

  // Copied from the golden output of decisiontx Ask.Content() (ask_test.go).
  const goldenAsk = "**Context**\nA change is ready. You decide when it ships.\n\n**Decision**\nShip now?\n\n**What is at stake**\nA bad change could reach users.\n\n**Options**\n- **NOW**: NOW happens.\n  - Pro: Pro\n  - Con: Con\n  - Cost or time: No extra time\n- **LATER**: LATER happens.\n  - Pro: Pro\n  - Con: Con\n  - Cost or time: No extra time\n\n**Recommendation**\nLATER\n\n**Default if no reply**\nLATER\n\n<details>\n<summary>Details</summary>\n\n**Agent did**\nRan checks.\n\n</details>\n\nInvestigate in issue: [Issue](mention://issue/11111111-1111-4111-8111-111111111111) · [Comment](/issue/11111111-1111-4111-8111-111111111111#comment-22222222-2222-4222-8222-222222222222)";

  const renderCard = (ask: string) => renderWithI18n(<QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={{ ...card, ask }} /></WorkspaceSlugProvider></QueryClientProvider>);

  it("renders the ELI10 ask in order with Details collapsed", () => {
    const { container } = renderCard(goldenAsk);
    const text = container.textContent ?? "";
    const marks = ["Context", "Options", "Recommendation", "Default if no reply", "Details", "Ran checks.", "Investigate in issue"].map((mark) => text.indexOf(mark));
    expect(marks.every((at, index) => at >= 0 && at > (marks[index - 1] ?? -1))).toBe(true);
    const details = container.querySelector("details");
    expect(details).not.toBeNull();
    expect(details).not.toHaveAttribute("open");
    expect(details?.querySelector("summary")).toHaveTextContent("Details");
    expect(details).toHaveTextContent("Ran checks.");
    expect(details).not.toHaveTextContent("Investigate in issue");
    expect(text.match(/Investigate in issue/g)).toHaveLength(1);
  });

  it("renders a legacy ask unchanged", () => {
    const legacy = "**Agent did**\nSwept PRs.\n\n**Decision**\nKeep gates?\n\n**Options**\n- KEEP\n- WAIVE";
    const { container } = renderCard(legacy);
    expect(container.querySelector("details")).toBeNull();
    expect(container).toHaveTextContent("Swept PRs.");
    expect(container).toHaveTextContent("Keep gates?");
    expect(container.querySelectorAll("li")).toHaveLength(2);
  });
});
