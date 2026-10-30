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
vi.mock("@multica/core/paths", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/paths")>()),
  useWorkspaceSlug: () => "acme",
  useWorkspacePaths: () => ({ chatSession: (id: string) => `/acme/chat?session=${id}`, issueDetail: (id: string) => `/acme/issues/${id}` }),
}));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: (_type: string, id: string) => (id === "agent-1" ? "x99-claude-sonnet" : id) }),
}));
vi.mock("../../navigation", async (importActual) => ({
  ...(await importActual<typeof import("../../navigation")>()),
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

function renderCard(entry: TimelineEntry, currentUserId = "human-1", compact = false) {
  const qc = new QueryClient();
  const invalidate = vi.spyOn(qc, "invalidateQueries");
  const view = renderWithI18n(
    <QueryClientProvider client={qc}>
      <DecisionQuoteCard entry={entry} issueId="issue-1" currentUserId={currentUserId} compact={compact} />
    </QueryClientProvider>,
  );
  return { ...view, invalidate };
}

beforeEach(() => {
  calls.length = 0;
  outcome = async () => ({});
});

describe("DecisionQuoteCard", () => {
  it("names the chosen option, shows Delivered and offers Jump to card", () => {
    renderCard(quote({ decision_state: "effective", decision_delivered: true, decision_chosen_option_index: 1, decision_option_label: "Hold" }));
    const card = screen.getByTestId("decision-quote-card");
    expect(card).toHaveTextContent("Option 2");
    expect(card).toHaveTextContent("Delivered");
    expect(screen.getByTestId("decision-quote-asked-dm")).toHaveTextContent("Jump to card");
  });

  it("shows the human's words, the state and the recorder as a sub-line", () => {
    renderCard(quote({ content: "Agent note here.\n\n> Yes, ship it.\n> Keep the words." }));
    const card = screen.getByTestId("decision-quote-card");
    expect(card).toHaveAttribute("data-state", "pending");
    expect(card).toHaveTextContent("Yes, ship it.");
    expect(card).toHaveTextContent("Keep the words.");
    expect(card).not.toHaveTextContent("> Yes");
    expect(card).toHaveTextContent("reversible");
    expect(card).toHaveTextContent("Recorded");
    expect(card).not.toHaveTextContent("Pending");
    expect(card).toHaveTextContent("x99-claude-sonnet");
    expect(card).toHaveTextContent("Agent note here.");
  });

  it("renders Markdown in the quoted words", () => {
    renderCard(quote({ content: "> **Ship** it\n>\n> - one\n> - two" }));
    const words = screen.getByTestId("decision-quote-words");
    expect(words.querySelector("strong")).toHaveTextContent("Ship");
    expect(words.querySelectorAll("li")).toHaveLength(2);
    expect(words).not.toHaveTextContent("**");
  });

  it("keeps the agent's note out of the quote: labelled, collapsed, Markdown", () => {
    renderCard(quote({ content: "Cheese decided: **stop Jev**.\n\n> Yes." }));
    const note = screen.getByTestId("decision-agent-note");
    expect(note.tagName).toBe("DETAILS");
    expect(note).not.toHaveAttribute("open");
    expect(note).toHaveTextContent("Agent's note");
    expect(note.querySelector("strong")).toHaveTextContent("stop Jev");
    expect(screen.getByTestId("decision-quote-words")).not.toHaveTextContent("stop Jev");
    expect(screen.getByTestId("decision-quote-words")).toHaveTextContent("Yes.");
  });

  it("omits the agent's note when there is none", () => {
    renderCard(quote());
    expect(screen.queryByTestId("decision-agent-note")).toBeNull();
  });

  it("shows the answered option label and a collapsed Full message for an excerpted record", () => {
    renderCard(quote({ content: "> yes", decision_answered_label: "D1", decision_full_message: "> D1: OK?\n\nyes\n\n> D2: Weekly?\n\nA" }));
    expect(screen.getByTestId("decision-answered-label")).toHaveTextContent("D1");
    expect(screen.getByTestId("decision-quote-words")).toHaveTextContent("yes");
    expect(screen.getByTestId("decision-quote-words")).not.toHaveTextContent("Weekly");
    const full = screen.getByTestId("decision-full-message");
    expect(full.tagName).toBe("DETAILS");
    expect(full).not.toHaveAttribute("open");
    expect(full).toHaveTextContent("Full message");
    expect(full).toHaveTextContent("Weekly?");
  });

  it("offers no Full message and no label on a whole-message record", () => {
    renderCard(quote());
    expect(screen.queryByTestId("decision-full-message")).toBeNull();
    expect(screen.queryByTestId("decision-answered-label")).toBeNull();
  });

  it("says when a pending record takes effect, in ICT, with the disclaim window", () => {
    renderCard(quote({ decision_state: "pending", decision_answered_at: "2026-10-30T03:00:00Z", effective_at: "2026-10-30T03:10:00Z" }));
    const timing = screen.getByTestId("decision-timing");
    expect(timing).toHaveTextContent("Pending");
    expect(timing).toHaveTextContent("10:10 ICT");
    expect(timing).toHaveTextContent("10 min to disclaim");
  });

  it("says a reversible pending record takes effect as soon as the agent is woken", () => {
    renderCard(quote({ decision_state: "pending", decision_answered_at: "2026-10-30T03:00:00Z", effective_at: "2026-10-30T03:00:00Z" }));
    const timing = screen.getByTestId("decision-timing");
    expect(timing).toHaveTextContent("as soon as the agent is woken");
    expect(timing).not.toHaveTextContent("to disclaim");
  });

  it("says when an effective record took effect, in ICT", () => {
    renderCard(quote({ decision_state: "effective", decision_delivered: true, decision_woken_at: "2026-10-30T04:05:00Z" }));
    expect(screen.getByTestId("decision-timing")).toHaveTextContent("In effect since 30 Oct, 11:05 ICT");
  });

  it("says when a disclaimed record was withdrawn, in ICT", () => {
    renderCard(quote({ decision_state: "disclaimed", disclaimed_at: "2026-10-30T05:30:00Z" }));
    expect(screen.getByTestId("decision-timing")).toHaveTextContent("Disclaimed at 30 Oct, 12:30 ICT");
  });

  it("shows Recorded, never In effect, for a woken answer the agent never received", () => {
    renderCard(quote({ decision_state: "effective", decision_delivered: false }));
    expect(screen.getByTestId("decision-quote-card")).toHaveTextContent("Recorded");
    expect(screen.getByTestId("decision-quote-card")).not.toHaveTextContent("In effect");
  });

  it("puts the receipt on the member's own comment: no quote body, Option N, Jump to card, Disclaim", async () => {
    renderCard(
      quote({
        actor_type: "member",
        actor_id: "human-1",
        content: "Hold.",
        decision_comment_kind: undefined,
        decision_answer_id: undefined,
        decision_receipt_answer_id: "answer-9",
        decision_issue_id: "issue-7",
        decision_issue_identifier: "CHE-77",
        decision_chosen_option_index: 0,
        decision_option_label: "Hold",
      }),
      "human-1",
      true,
    );
    const card = screen.getByTestId("decision-quote-card");
    expect(card).toHaveTextContent("Recorded");
    expect(card).toHaveTextContent("Option 1");
    expect(card).not.toHaveTextContent("Hold.");
    expect(screen.getByTestId("decision-receipt-issue")).toHaveTextContent("Recorded for CHE-77");
    expect(screen.getByTestId("decision-receipt-issue")).toHaveAttribute("href", "/acme/issues/issue-7");
    expect(screen.getByTestId("decision-quote-asked-dm")).toHaveTextContent("Jump to card");
    fireEvent.click(screen.getByTestId("decision-disclaim-button"));
    fireEvent.click(await screen.findByRole("button", { name: "Disclaim" }));
    await waitFor(() => expect(calls).toEqual(["answer-9"]));
  });

  it("gives the request's human an Open DM link and a Disclaim button", () => {
    renderCard(quote());
    expect(screen.getByTestId("decision-quote-open-dm")).toHaveAttribute("href", "/acme/chat?session=session-1&message=msg-1");
    expect(screen.getByTestId("decision-disclaim-button")).toBeEnabled();
  });

  it("links the request card and the answering message, each in its own DM session", () => {
    renderCard(quote());
    expect(screen.getByTestId("decision-quote-asked-dm")).toHaveAttribute("href", "/acme/chat?session=session-0&message=ask-1");
    expect(screen.getByTestId("decision-quote-asked-dm")).toHaveTextContent("Jump to card");
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
