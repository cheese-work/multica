import { act, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderWithI18n } from "../../test/i18n";
import { useDecisionAnswerDraftStore } from "@multica/core/issues/stores/decision-answer-draft-store";
import { IssueDecisionBanner } from "./issue-decision-banner";

vi.mock("@multica/core/hooks", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/hooks")>()),
  useWorkspaceId: () => "ws",
}));
vi.mock("../../navigation", async (importActual) => ({
  ...(await importActual<typeof import("../../navigation")>()),
  AppLink: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => <a href={href} {...rest}>{children}</a>,
}));

const api = vi.hoisted(() => ({
  listOpenDecisionRequests: vi.fn(),
  listIssueDecisionCards: vi.fn(),
  listTasksByIssue: vi.fn(),
  answerDecisionRequest: vi.fn(),
  listDecisionArtifacts: vi.fn(),
}));
vi.mock("@multica/core/api", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/api")>()),
  api,
}));

const open = (id: string, issue_id: string, extra = {}) => ({
  id, issue_id, issue_identifier: "CHE-1", asking_agent_id: "a", asking_agent_name: "Terra",
  ask: `Ask ${id}`, escalation_class: "spend", created_at: "2026-11-01T05:00:00Z",
  chat_session_id: "s", message_id: null, source_comment_id: "c1",
  options: [{ label: "Ship it" }, { label: "Hold" }], ...extra,
});

vi.mock("@multica/core/workspace/queries", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/workspace/queries")>()),
  agentListOptions: () => ({ queryKey: ["agents"], queryFn: async () => [{ id: "owner", name: "Terra" }, { id: "rev", name: "Sol" }] }),
}));

function show(status = "in_progress", ownerAgentId: string | null = "owner") {
  return renderWithI18n(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <WorkspaceSlugProvider slug="acme"><IssueDecisionBanner issueId="issue" status={status} ownerAgentId={ownerAgentId} /></WorkspaceSlugProvider>
    </QueryClientProvider>,
  );
}

describe("IssueDecisionBanner", () => {
  beforeEach(() => {
    vi.resetAllMocks();
    useDecisionAnswerDraftStore.setState({ drafts: {} });
    api.listDecisionArtifacts.mockResolvedValue([]);
    api.listIssueDecisionCards.mockResolvedValue([]);
    api.listTasksByIssue.mockResolvedValue([]);
  });

  it("shows an open request with working option buttons", async () => {
    api.listOpenDecisionRequests.mockResolvedValue([open("r1", "issue"), open("other", "elsewhere")]);
    api.answerDecisionRequest.mockResolvedValue({});
    show();
    expect(await screen.findByRole("region", { name: "Decision needed" })).toBeInTheDocument();
    expect(screen.getByText("Ask r1")).toBeInTheDocument();
    expect(screen.queryByText("Ask other")).toBeNull();
    expect(screen.getByRole("link", { name: "Source comment" })).toHaveAttribute("href", expect.stringContaining("#comment-c1"));
    const user = userEvent.setup();
    await user.click(screen.getByRole("radio", { name: /Hold/ }));
    expect(api.answerDecisionRequest).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(api.answerDecisionRequest).toHaveBeenCalledWith("r1", { option: "Hold" }));
  });

  it("shows the covered decisions of a batch card on the issue page", async () => {
    api.listOpenDecisionRequests.mockResolvedValue([open("batch", "issue", {
      covers_request_ids: ["c1", "c2"],
      covered_cards: [
        { id: "c1", issue_id: "i1", issue_identifier: "CHE-11", decision: "Merge the stack?", options: [{ label: "Ship it", what_happens: "Merges." }, { label: "Hold" }] },
        { id: "c2", issue_id: "i2", issue_identifier: "CHE-12", decision: "Cut the release?", options: [{ label: "Ship it", what_happens: "Cuts." }, { label: "Hold" }] },
      ],
    })]);
    show();
    const items = await screen.findAllByTestId("decision-request-covered-item");
    expect(items).toHaveLength(2);
    expect(items[0]).toHaveTextContent("CHE-11: Merge the stack?");
    expect(items[1]).toHaveTextContent("CHE-12: Cut the release?");
    expect(items[1]).toHaveTextContent("Ship it: Cuts.");
    expect(screen.getByRole("radio", { name: /Hold/ })).toBeInTheDocument();
  });

  it("acknowledges the covered cards when a batch card is answered on the issue page", async () => {
    api.listOpenDecisionRequests.mockResolvedValue([open("batch", "issue", {
      covers_request_ids: ["c2", "c1"],
      covered_cards: [
        { id: "c2", issue_id: "i2", issue_identifier: "CHE-12", decision: "Cut the release?", options: [{ label: "Ship it", what_happens: "Cuts." }, { label: "Hold" }] },
        { id: "c1", issue_id: "i1", issue_identifier: "CHE-11", decision: "Merge the stack?", options: [{ label: "Ship it", what_happens: "Merges." }, { label: "Hold" }] },
      ],
    })]);
    show();
    const user = userEvent.setup();
    await user.click(await screen.findByRole("radio", { name: /Hold/ }));
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(api.answerDecisionRequest).toHaveBeenCalledTimes(1));
    expect(api.answerDecisionRequest.mock.calls[0]?.[0]).toBe("batch");
    expect(api.answerDecisionRequest.mock.calls[0]?.[1]).toEqual({ option: "Hold", covers_ack: expect.stringMatching(/^[0-9a-f]{64}$/) });
  });

  it("offers no answer on the issue page while a batch card's covered decisions are missing", async () => {
    api.listOpenDecisionRequests.mockResolvedValue([open("batch", "issue", { covers_request_ids: ["c1", "c2"] })]);
    show();
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Hold/ })).toBeNull();
    expect(screen.queryByTestId("decision-request-answer")).toBeNull();
    expect(api.answerDecisionRequest).not.toHaveBeenCalled();
  });

  it("does not tell the reader about the DM message box on the issue page", async () => {
    api.listOpenDecisionRequests.mockResolvedValue([open("r1", "issue")]);
    show();
    expect(await screen.findByText("Send posts your answer to CHE-1.")).toBeInTheDocument();
    expect(screen.queryByText(/DM agent/)).toBeNull();
  });

  it("lists several open requests under one banner with a count", async () => {
    api.listOpenDecisionRequests.mockResolvedValue([open("r1", "issue"), open("r2", "issue")]);
    show();
    expect(await screen.findByText("2")).toBeInTheDocument();
    expect(screen.getAllByTestId("decision-request-card")).toHaveLength(2);
  });

  it("explains need_decision with no open request and shows the owner run", async () => {
    api.listOpenDecisionRequests.mockResolvedValue([]);
    api.listTasksByIssue.mockResolvedValue([
      { id: "t", issue_id: "issue", agent_id: "owner", status: "queued", created_at: "2026-11-01T05:24:00Z" },
    ]);
    show("need_decision");
    expect(await screen.findByText(/has not sent the decision card yet/)).toBeInTheDocument();
    expect(await screen.findByText(/Queued since 01 Nov 2026, 12:24 ICT/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Send" })).toBeNull();
  });

  it("shows the owner's run only, named, not an unrelated reviewer's", async () => {
    api.listOpenDecisionRequests.mockResolvedValue([]);
    api.listTasksByIssue.mockResolvedValue([
      { id: "t1", issue_id: "issue", agent_id: "rev", status: "running", started_at: "2026-11-01T05:30:00Z", created_at: "2026-11-01T05:30:00Z" },
      { id: "t2", issue_id: "issue", agent_id: "owner", status: "queued", created_at: "2026-11-01T05:24:00Z" },
    ]);
    show("need_decision");
    expect(await screen.findByText(/Terra.*Queued since 01 Nov 2026, 12:24 ICT/)).toBeInTheDocument();
    expect(screen.queryByText(/Running since/)).toBeNull();
    expect(screen.queryByText(/Sol/)).toBeNull();
  });

  it("says the owner has no active run only once tasks loaded, not while pending or failed", async () => {
    api.listOpenDecisionRequests.mockResolvedValue([]);
    api.listTasksByIssue.mockRejectedValue(new Error("boom"));
    show("need_decision");
    expect(await screen.findByText(/has not sent the decision card yet/)).toBeInTheDocument();
    await waitFor(() => expect(api.listTasksByIssue).toHaveBeenCalled());
    expect(screen.queryByText("No active run.")).toBeNull();
  });

  it("shows a retryable error when open requests fail to load", async () => {
    api.listOpenDecisionRequests.mockRejectedValueOnce(new Error("boom")).mockResolvedValue([open("r1", "issue")]);
    show("need_decision");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not load open asks.");
    await userEvent.setup().click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Ask r1")).toBeInTheDocument();
  });

  it("keeps the retryable error when a refresh fails after an empty success", async () => {
    api.listOpenDecisionRequests.mockResolvedValueOnce([]).mockRejectedValueOnce(new Error("boom"));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderWithI18n(
      <QueryClientProvider client={client}>
        <WorkspaceSlugProvider slug="acme"><IssueDecisionBanner issueId="issue" status="need_decision" ownerAgentId="owner" /></WorkspaceSlugProvider>
      </QueryClientProvider>,
    );
    expect(await screen.findByText(/has not sent the decision card yet/)).toBeInTheDocument();
    await act(() => client.refetchQueries({ queryKey: ["chat", "ws", "open-decisions"] }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not load open asks.");
  });

  it("renders nothing when the issue has no decision cards at all", async () => {
    api.listOpenDecisionRequests.mockResolvedValue([]);
    const { container } = show("in_progress");
    await waitFor(() => expect(api.listIssueDecisionCards).toHaveBeenCalledWith("issue"));
    expect(container).toBeEmptyDOMElement();
  });

  describe("used cards", () => {
    const used = (id: string, extra = {}) => ({
      ...open(id, "issue"), state: "answered", record_comment_id: "rec",
      answer: { state: "picked_up", chosen_option_index: 0, option_label: "Ship it", answered_at: "2026-11-01T05:10:00Z", run: { id: "t", agent_name: "Terra" } },
      ...extra,
    });
    const rowOf = (id: string) => screen.getByRole("button", { name: new RegExp(`Ask ${id}`) });

    it.each([
      ["picked_up", used("done")],
      ["withdrawn", used("done", { state: "withdrawn", answer: undefined, withdrawn_at: "2026-11-01T05:20:00Z" })],
      ["disclaimed", used("done", { answer: { state: "disclaimed", answered_at: "2026-11-01T05:10:00Z" } })],
    ])("loads a %s card folded: one row, collapsed, body inert", async (_, card) => {
      api.listOpenDecisionRequests.mockResolvedValue([]);
      api.listIssueDecisionCards.mockResolvedValue([card]);
      show();
      expect(await screen.findByRole("region", { name: "Decisions" })).toBeInTheDocument();
      expect(rowOf("done")).toHaveAttribute("aria-expanded", "false");
      expect(screen.getByTestId("decision-request-card").querySelector("[data-fold-body]")).toHaveAttribute("inert");
    });

    it("expands a folded card on click", async () => {
      api.listOpenDecisionRequests.mockResolvedValue([]);
      api.listIssueDecisionCards.mockResolvedValue([used("done")]);
      show();
      const user = userEvent.setup();
      await user.click(await screen.findByRole("button", { name: /Ask done/ }));
      expect(rowOf("done")).toHaveAttribute("aria-expanded", "true");
    });

    it("keeps a not_picked_up card expanded", async () => {
      api.listOpenDecisionRequests.mockResolvedValue([]);
      api.listIssueDecisionCards.mockResolvedValue([used("late", { answer: { state: "delivered", chosen_option_index: 0, option_label: "Ship it", answered_at: "2026-11-01T05:00:00Z" } })]);
      show();
      expect(await screen.findByTestId("decision-request-card")).toHaveAttribute("data-state", "not_picked_up");
      expect(screen.queryByTestId("decision-request-fold-toggle")).toBeNull();
    });

    it("shows open and used cards together, open first and expanded", async () => {
      api.listOpenDecisionRequests.mockResolvedValue([open("new", "issue")]);
      api.listIssueDecisionCards.mockResolvedValue([used("old")]);
      show();
      const cards = await screen.findAllByTestId("decision-request-card");
      expect(cards).toHaveLength(2);
      expect(cards[0]).toHaveAttribute("data-state", "open");
      expect(cards[1]).toHaveAttribute("data-state", "picked_up");
    });

    it("folds the same card in place when its answer is picked up", async () => {
      api.listOpenDecisionRequests.mockResolvedValue([open("r1", "issue")]);
      const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
      renderWithI18n(
        <QueryClientProvider client={client}>
          <WorkspaceSlugProvider slug="acme"><IssueDecisionBanner issueId="issue" status="in_progress" ownerAgentId="owner" /></WorkspaceSlugProvider>
        </QueryClientProvider>,
      );
      const card = await screen.findByTestId("decision-request-card");
      expect(card).toHaveAttribute("data-state", "open");
      api.listOpenDecisionRequests.mockResolvedValue([]);
      api.listIssueDecisionCards.mockResolvedValue([used("r1")]);
      await act(() => client.invalidateQueries());
      await waitFor(() => expect(rowOf("r1")).toHaveAttribute("aria-expanded", "false"));
      expect(screen.getByTestId("decision-request-card")).toBe(card);
    });

    it("does not unmount a card between the two refetches of one answer", async () => {
      api.listOpenDecisionRequests.mockResolvedValue([open("r1", "issue")]);
      const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
      renderWithI18n(
        <QueryClientProvider client={client}>
          <WorkspaceSlugProvider slug="acme"><IssueDecisionBanner issueId="issue" status="in_progress" ownerAgentId="owner" /></WorkspaceSlugProvider>
        </QueryClientProvider>,
      );
      const card = await screen.findByTestId("decision-request-card");
      // The open list answers first and no longer has the card; the used list is still in flight.
      let finishUsed: (cards: unknown[]) => void = () => {};
      api.listOpenDecisionRequests.mockResolvedValue([]);
      api.listIssueDecisionCards.mockReturnValue(new Promise((resolve) => { finishUsed = resolve; }));
      await act(() => { void client.invalidateQueries(); });
      await waitFor(() => expect(api.listIssueDecisionCards).toHaveBeenCalledTimes(2));
      expect(screen.getByTestId("decision-request-card")).toBe(card);
      await act(async () => finishUsed([used("r1")]));
      await waitFor(() => expect(rowOf("r1")).toHaveAttribute("aria-expanded", "false"));
      expect(screen.getByTestId("decision-request-card")).toBe(card);
    });

    it("titles an all-used banner neutrally with the answered count", async () => {
      api.listOpenDecisionRequests.mockResolvedValue([]);
      api.listIssueDecisionCards.mockResolvedValue([used("a"), used("b")]);
      show();
      const region = await screen.findByRole("region", { name: "Decisions" });
      expect(region).toHaveTextContent("2 answered");
      expect(region.className).not.toMatch(/warning/);
    });
  });
});
