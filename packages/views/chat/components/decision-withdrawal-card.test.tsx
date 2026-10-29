import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { renderWithI18n } from "../../test/i18n";
import { DecisionWithdrawalCard } from "./decision-withdrawal-card";

vi.mock("../../navigation", async (importActual) => ({
  ...(await importActual<typeof import("../../navigation")>()),
  AppLink: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => <a href={href} {...rest}>{children}</a>,
}));

const card = {
  id: "request", issue_id: "issue", issue_identifier: "CHE-2234",
  source_comment_id: "source", ask: "Original ask", state: "withdrawn" as const,
  created_at: "2026-10-27T10:11:49Z", reason: "Scope changed. ".repeat(20),
};

describe("DecisionWithdrawalCard accessibility", () => {
  it("exposes a keyboard-accessible source link and the full truncated reason", async () => {
    renderWithI18n(<WorkspaceSlugProvider slug="acme"><DecisionWithdrawalCard card={card} /></WorkspaceSlugProvider>);
    const user = userEvent.setup();
    await user.tab();
    expect(screen.getByRole("link", { name: "Withdrawn in CHE-2234" })).toHaveFocus();
    const reason = screen.getByTitle(card.reason.trim());
    expect(reason).toHaveTextContent(card.reason.trim());
    expect(reason).toHaveAttribute("title", card.reason);
    expect(reason).toHaveClass("truncate", "text-muted-foreground");
    expect(screen.getByTestId("decision-withdrawal-card")).toHaveAttribute("data-state", "withdrawn");
  });

  it("localizes the announcement and tolerates a missing legacy reason", () => {
    renderWithI18n(<WorkspaceSlugProvider slug="acme"><DecisionWithdrawalCard card={{ ...card, reason: undefined }} /></WorkspaceSlugProvider>, { locale: "zh-Hans" });
    expect(screen.getByRole("region", { name: "已在 CHE-2234 撤回" })).toHaveTextContent("已撤回");
    expect(screen.queryByText(card.ask)).toBeNull();
    expect(screen.queryByTitle(card.reason)).toBeNull();
  });
});
