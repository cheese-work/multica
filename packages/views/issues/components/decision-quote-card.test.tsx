import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApiError } from "@multica/core/api/client";
import type { TimelineEntry } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { DecisionQuoteCard } from "./decision-quote-card";

// A plain function, not vi.fn(): vitest derives a promise from a mock's rejected
// result that nothing handles, which fails the test for an error the component
// already caught.
const calls: string[] = [];
let outcome: () => Promise<unknown> = async () => ({});
const disclaim = (id: string) => {
  calls.push(id);
  return outcome();
};
vi.mock("@multica/core/issues/decision-disclaim", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/issues/decision-disclaim")>()),
  useDisclaimDecisionAnswer: () => ({ mutateAsync: disclaim, isPending: false }),
}));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ chatSession: (id: string) => `/acme/chat?session=${id}` }),
}));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: (_type: string, id: string) => (id === "agent-1" ? "x99-claude-sonnet" : id) }),
}));
vi.mock("../../navigation", () => ({
  AppLink: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => (
    <a href={href} {...rest}>{children}</a>
  ),
}));

function quote(overrides: Partial<TimelineEntry> = {}): TimelineEntry {
  return {
    type: "comment",
    id: "c1",
    actor_type: "agent",
    actor_id: "agent-1",
    created_at: "2026-10-23T05:00:00Z",
    content: "> Yes, ship it.\n> Keep the words.",
    decision_comment_kind: "quote",
    decision_answer_id: "answer-1",
    decision_human_user_id: "human-1",
    decision_recorder_agent_id: "agent-1",
    decision_class: "reversible",
    decision_state: "pending",
    decision_source_session_id: "session-1",
    decision_source_message_id: "msg-1",
    decision_request_session_id: "session-0",
    decision_request_message_id: "ask-1",
    ...overrides,
  };
}

function renderCard(entry: TimelineEntry, currentUserId = "human-1") {
  const qc = new QueryClient();
  const invalidate = vi.spyOn(qc, "invalidateQueries");
  const view = renderWithI18n(
    <QueryClientProvider client={qc}>
      <DecisionQuoteCard entry={entry} issueId="issue-1" currentUserId={currentUserId} />
    </QueryClientProvider>,
  );
  return { ...view, invalidate };
}

beforeEach(() => {
  calls.length = 0;
  outcome = async () => ({});
});

describe("DecisionQuoteCard", () => {
  it("shows the human's words, the state and the recorder as a sub-line", () => {
    renderCard(quote({ content: "Agent note here.\n\n> Yes, ship it.\n> Keep the words." }));
    const card = screen.getByTestId("decision-quote-card");
    expect(card).toHaveAttribute("data-state", "pending");
    expect(card).toHaveTextContent("Yes, ship it.");
    expect(card).toHaveTextContent("Keep the words.");
    expect(card).not.toHaveTextContent("> Yes");
    expect(card).toHaveTextContent("reversible");
    expect(card).toHaveTextContent("Pending");
    expect(card).toHaveTextContent("x99-claude-sonnet");
    expect(card).toHaveTextContent("Agent note here.");
  });

  it("gives the request's human an Open DM link and a Disclaim button", () => {
    renderCard(quote());
    expect(screen.getByTestId("decision-quote-open-dm")).toHaveAttribute("href", "/acme/chat?session=session-1&message=msg-1");
    expect(screen.getByTestId("decision-disclaim-button")).toBeEnabled();
  });

  it("links the request card and the answering message, each in its own DM session", () => {
    renderCard(quote());
    expect(screen.getByTestId("decision-quote-asked-dm")).toHaveAttribute("href", "/acme/chat?session=session-0&message=ask-1");
    expect(screen.getByTestId("decision-quote-asked-dm")).toHaveTextContent("Asked in DM");
    expect(screen.getByTestId("decision-quote-open-dm")).toHaveAttribute("href", "/acme/chat?session=session-1&message=msg-1");
    expect(screen.getByTestId("decision-quote-open-dm")).toHaveTextContent("Answered");
  });

  it("omits the asked link when the request card is unknown", () => {
    renderCard(quote({ decision_request_session_id: undefined, decision_request_message_id: undefined }));
    expect(screen.queryByTestId("decision-quote-asked-dm")).toBeNull();
    expect(screen.getByTestId("decision-quote-open-dm")).toBeInTheDocument();
  });

  it("gives another member neither a link nor Disclaim, and says the source is private", () => {
    renderCard(quote({ decision_source_session_id: undefined, decision_source_message_id: undefined, decision_request_session_id: undefined, decision_request_message_id: undefined }), "member-2");
    expect(screen.queryByTestId("decision-quote-open-dm")).toBeNull();
    expect(screen.queryByTestId("decision-quote-asked-dm")).toBeNull();
    expect(screen.queryByTestId("decision-disclaim-button")).toBeNull();
    expect(screen.getByTestId("decision-quote-card")).toHaveTextContent("from a private DM");
  });

  it("mutes a disclaimed card and removes Disclaim", () => {
    renderCard(quote({ decision_state: "disclaimed" }));
    expect(screen.getByTestId("decision-quote-card")).toHaveAttribute("data-state", "disclaimed");
    expect(screen.getByTestId("decision-quote-card")).toHaveTextContent("Disclaimed");
    expect(screen.queryByTestId("decision-disclaim-button")).toBeNull();
  });

  it("confirms Disclaim in a dialog that has no reason field, then withdraws", async () => {
        renderCard(quote());
    fireEvent.click(screen.getByTestId("decision-disclaim-button"));
    expect(await screen.findByRole("alertdialog")).toBeInTheDocument();
    expect(screen.queryByRole("textbox")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Disclaim" }));
    await waitFor(() => expect(calls).toEqual(["answer-1"]));
  });

  it("shows the changed-context notice, disables Disclaim and offers Refresh, not a retry", async () => {
    outcome = async () => { throw new ApiError("decision context changed; retry", 409, "Conflict"); };
    const { invalidate } = renderCard(quote());
    fireEvent.click(screen.getByTestId("decision-disclaim-button"));
    fireEvent.click(await screen.findByRole("button", { name: "Disclaim" }));
    const notice = await screen.findByTestId("decision-conflict-notice");
    expect(notice).toHaveAttribute("role", "alert");
    expect(notice).toHaveTextContent("changed while you were reading");
    expect(notice).toHaveTextContent("Nothing was withdrawn");
    expect(screen.getByTestId("decision-disclaim-button")).toBeDisabled();
    expect(screen.queryByRole("button", { name: /try again|retry/i })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    expect(invalidate).toHaveBeenCalled();
    await waitFor(() => expect(screen.queryByTestId("decision-conflict-notice")).toBeNull());
    expect(screen.getByTestId("decision-disclaim-button")).toBeEnabled();
  });

  it("shows the already-withdrawn notice, mutes the card and offers Review", async () => {
    outcome = async () => { throw new ApiError("decision answer is already withdrawn", 409, "Conflict"); };
    renderCard(quote());
    fireEvent.click(screen.getByTestId("decision-disclaim-button"));
    fireEvent.click(await screen.findByRole("button", { name: "Disclaim" }));
    const notice = await screen.findByTestId("decision-conflict-notice");
    expect(notice).toHaveAttribute("role", "status");
    expect(notice).toHaveTextContent("Already withdrawn");
    expect(notice).toHaveTextContent("Nothing was changed by this click");
    expect(screen.getByTestId("decision-quote-card")).toHaveAttribute("data-state", "disclaimed");
    expect(screen.queryByTestId("decision-disclaim-button")).toBeNull();
    expect(screen.getByRole("button", { name: "Review" })).toBeInTheDocument();
  });

  it("keeps a generic failure out of the conflict notices", async () => {
    outcome = async () => { throw new ApiError("boom", 500, "Server Error"); };
    renderCard(quote());
    fireEvent.click(screen.getByTestId("decision-disclaim-button"));
    fireEvent.click(await screen.findByRole("button", { name: "Disclaim" }));
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(screen.queryByTestId("decision-conflict-notice")).toBeNull();
    expect(screen.getByTestId("decision-disclaim-button")).toBeEnabled();
  });
});
