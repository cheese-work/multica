import { screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Issue } from "@multica/core/types";
import { issueListOptions } from "@multica/core/issues/queries";
import { renderWithI18n } from "../../test/i18n";
import { DecisionRequestCard } from "./decision-request-card";

const WORKSPACE_ID = "workspace-1";
const ISSUE_ID = "11111111-1111-4111-8111-111111111111";

vi.mock("@multica/core/hooks", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/hooks")>()),
  useWorkspaceId: () => WORKSPACE_ID,
}));

vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () => ({ iconOf: () => null, colorOf: () => null }),
}));

vi.mock("../../navigation", async (importActual) => ({
  ...(await importActual<typeof import("../../navigation")>()),
  AppLink: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => <a href={href} {...rest}>{children}</a>,
}));

vi.mock("@multica/core/api", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/api")>()),
  api: { answerDecisionRequest: vi.fn(), listDecisionArtifacts: vi.fn().mockResolvedValue([]), uploadFile: vi.fn() },
}));

const card = {
  id: "request", issue_id: "issue", issue_identifier: "CHE-2204",
  ask: `Ship [CHE-2164](mention://issue/${ISSUE_ID}) now?`,
  state: "open" as const, created_at: "2026-10-26T23:30:00Z",
};

function renderCard(props: Partial<typeof card> & { issue_title?: string } = {}) {
  const client = new QueryClient();
  client.setQueryData(issueListOptions(WORKSPACE_ID).queryKey, {
    byStatus: { unstarted: { issues: [{ id: ISSUE_ID, identifier: "CHE-2164", title: "Parser rewrite for the inbox", status: "todo" }] as unknown as Issue[], total: 1 } },
  });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={{ ...card, ...props }} /></WorkspaceSlugProvider>
    </QueryClientProvider>,
  );
}

describe("DecisionRequestCard compact issue chips", () => {
  beforeEach(() => vi.clearAllMocks());

  it("shows the identifier only in the ask body and keeps the hover card", () => {
    renderCard();
    const chip = screen.getByTestId("decision-request-ask").querySelector(".issue-mention .issue-mention");
    expect(chip).not.toBeNull();
    expect(chip).toHaveTextContent(/^CHE-2164$/);
    expect(screen.queryByText("Parser rewrite for the inbox")).toBeNull();
    expect(chip?.closest('[data-slot="hover-card-trigger"]')).not.toBeNull();
  });

  it("shows the issue title next to the identifier in the Source row", () => {
    renderCard({ issue_title: "Decision cards stay readable" });
    expect(screen.getByTestId("decision-request-source-issue")).toHaveTextContent("CHE-2204");
    expect(screen.getByTestId("decision-request-source-title")).toHaveTextContent("Decision cards stay readable");
  });

  it("folds a card a batch answer closed into one row", () => {
    renderCard({ state: "answered", answered_by_batch_request_id: "batch-1", issue_title: "Decision cards stay readable" } as never);
    expect(screen.getByTestId("decision-request-folded")).toHaveTextContent("CHE-2204: answered by the batch card");
    expect(screen.queryByTestId("decision-request-card")).toBeNull();
  });

  it("tells a batch card how many cards its answer closes", () => {
    renderCard({ covers_request_ids: ["a", "b", "c"] } as never);
    expect(screen.getByTestId("decision-request-covers")).toHaveTextContent("Closed with this answer: 3");
  });

  it("lists every decision a batch answer closes, so no covered decision is hidden", () => {
    renderCard({
      covers_request_ids: ["a", "b"],
      covered_cards: [
        { id: "a", issue_id: "i1", issue_identifier: "CHE-1", issue_title: "Small fix", decision: "Hold or proceed?" },
        { id: "b", issue_id: "i2", issue_identifier: "CHE-2", issue_title: "Redesign", decision: "Ship the redesign?" },
      ],
    } as never);
    const items = screen.getAllByTestId("decision-request-covered-item");
    expect(items).toHaveLength(2);
    expect(items[0]).toHaveTextContent("CHE-1 Small fix: Hold or proceed?");
    expect(items[1]).toHaveTextContent("CHE-2 Redesign: Ship the redesign?");
  });

  it("shows each covered option with its meaning, so equal labels cannot hide different actions", () => {
    renderCard({
      covers_request_ids: ["a", "b"],
      covered_cards: [
        { id: "a", issue_id: "i1", issue_identifier: "CHE-1", decision: "Deploy?", options: [{ label: "Approve", what_happens: "Deploys the staging build." }] },
        { id: "b", issue_id: "i2", issue_identifier: "CHE-2", decision: "Deploy?", options: [{ label: "Approve", what_happens: "Deletes the production data." }] },
      ],
    } as never);
    const items = screen.getAllByTestId("decision-request-covered-item");
    expect(items[0]).toHaveTextContent("Approve: Deploys the staging build.");
    expect(items[1]).toHaveTextContent("Approve: Deletes the production data.");
  });

  it("offers no answer form while a covered decision is missing, and says why", () => {
    renderCard({ state: "open", covers_request_ids: ["a", "b"], covered_cards: [] } as never);
    expect(screen.getByRole("alert")).toHaveTextContent("Cannot load the decisions this answer would close");
    expect(screen.queryByTestId("decision-request-answer")).toBeNull();
  });
});
