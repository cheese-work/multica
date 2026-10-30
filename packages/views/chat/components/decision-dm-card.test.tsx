import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApiError } from "@multica/core/api/client";
import type { DecisionCard } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { DecisionDmCard } from "./decision-dm-card";

// A plain function, not vi.fn(): vitest derives an unhandled promise from a
// rejecting mock that nothing handles, although the component caught it.
const calls: string[] = [];
let outcome: () => Promise<unknown> = async () => ({});
vi.mock("@multica/core/issues/decision-disclaim", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/issues/decision-disclaim")>()),
  useDisclaimDecisionAnswer: () => ({
    mutateAsync: (id: string) => {
      calls.push(id);
      return outcome();
    },
    isPending: false,
  }),
}));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ issueDetail: (id: string) => `/acme/issues/${id}`, chatSession: (id: string) => `/acme/chat?session=${id}` }),
}));
vi.mock("../../navigation", () => ({
  AppLink: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => (
    <a href={href} {...rest}>{children}</a>
  ),
}));

const card = (overrides: Partial<DecisionCard> = {}): DecisionCard => ({
  answer_id: "answer-1",
  issue_id: "issue-1",
  issue_identifier: "CHE-1870",
  issue_comment_id: "comment-1",
  class: "reversible",
  state: "effective",
  effective_at: "2026-10-23T05:00:00Z",
  inherited: false,
  ...overrides,
});

function renderCard(c: DecisionCard) {
  const qc = new QueryClient();
  const invalidate = vi.spyOn(qc, "invalidateQueries");
  const view = renderWithI18n(
    <QueryClientProvider client={qc}>
      <DecisionDmCard card={c} />
    </QueryClientProvider>,
  );
  return { ...view, invalidate };
}

beforeEach(() => {
  calls.length = 0;
  outcome = async () => ({});
});

describe("DecisionDmCard", () => {
  it("says what was quoted, links the original issue comment and shows the state", () => {
    renderCard(card());
    const el = screen.getByTestId("decision-dm-card");
    expect(el).toHaveAttribute("data-state", "effective");
    expect(el).toHaveTextContent("Recorded for CHE-1870");
    expect(el).toHaveTextContent("Recorded");
    expect(el).not.toHaveTextContent("In effect");
    expect(screen.getByTestId("decision-dm-card-link")).toHaveAttribute("href", "/acme/issues/issue-1#comment-comment-1");
    expect(screen.getByTestId("decision-dm-disclaim")).toBeEnabled();
  });

  it("names the chosen option, shows Delivered once the agent was woken, and jumps back to the ask card", () => {
    renderCard(card({
      delivered: true, chosen_option_index: 1, option_label: "Ship now",
      request_session_id: "session-0", request_message_id: "ask-1",
    }));
    const el = screen.getByTestId("decision-dm-card");
    expect(el).toHaveTextContent("Recorded for CHE-1870");
    expect(el).toHaveTextContent("Option 2");
    expect(el).toHaveTextContent("Delivered");
    expect(screen.getByTestId("decision-dm-jump")).toHaveAttribute("href", "/acme/chat?session=session-0&message=ask-1");
    expect(screen.getByTestId("decision-dm-jump")).toHaveTextContent("Jump to card");
  });

  it("offers no jump when the ask card is unknown or the card is a disclaimed copy", () => {
    renderCard(card());
    expect(screen.queryByTestId("decision-dm-jump")).toBeNull();
  });

  it("is read-only provenance on a fork: labelled, still linked to the original comment, no Disclaim", () => {
    renderCard(card({ inherited: true }));
    const el = screen.getByTestId("decision-dm-card");
    expect(el).toHaveTextContent("from the original DM");
    expect(el).toHaveTextContent("Inherited");
    expect(screen.getByTestId("decision-dm-card-link")).toHaveAttribute("href", "/acme/issues/issue-1#comment-comment-1");
    expect(screen.queryByTestId("decision-dm-disclaim")).toBeNull();
  });

  it("mutes a disclaimed card and drops Disclaim", () => {
    renderCard(card({ state: "disclaimed" }));
    expect(screen.getByTestId("decision-dm-card")).toHaveAttribute("data-state", "disclaimed");
    expect(screen.queryByTestId("decision-dm-disclaim")).toBeNull();
  });

  it("confirms with no reason field, then withdraws", async () => {
    renderCard(card());
    fireEvent.click(screen.getByTestId("decision-dm-disclaim"));
    expect(await screen.findByRole("alertdialog")).toBeInTheDocument();
    expect(screen.queryByRole("textbox")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Disclaim" }));
    await waitFor(() => expect(calls).toEqual(["answer-1"]));
  });

  it("shows the changed-context notice, keeps Disclaim off until Refresh, and never retries", async () => {
    outcome = async () => { throw new ApiError("decision context changed; retry", 409, "Conflict"); };
    const { invalidate } = renderCard(card());
    fireEvent.click(screen.getByTestId("decision-dm-disclaim"));
    fireEvent.click(await screen.findByRole("button", { name: "Disclaim" }));
    const notice = await screen.findByTestId("decision-dm-conflict");
    expect(notice).toHaveAttribute("role", "alert");
    expect(notice).toHaveTextContent("changed while you were reading");
    expect(screen.getByTestId("decision-dm-disclaim")).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    expect(invalidate).toHaveBeenCalled();
    await waitFor(() => expect(screen.queryByTestId("decision-dm-conflict")).toBeNull());
    expect(screen.getByTestId("decision-dm-disclaim")).toBeEnabled();
  });

  it("mutes the card on an already-withdrawn answer and offers Review as a link to the issue comment", async () => {
    outcome = async () => { throw new ApiError("decision answer is already withdrawn", 409, "Conflict"); };
    renderCard(card());
    fireEvent.click(screen.getByTestId("decision-dm-disclaim"));
    fireEvent.click(await screen.findByRole("button", { name: "Disclaim" }));
    const notice = await screen.findByTestId("decision-dm-conflict");
    expect(notice).toHaveAttribute("role", "status");
    expect(notice).toHaveTextContent("Already withdrawn");
    expect(screen.getByTestId("decision-dm-card")).toHaveAttribute("data-state", "disclaimed");
    expect(screen.queryByTestId("decision-dm-disclaim")).toBeNull();
    expect(screen.getByRole("link", { name: "Review" })).toHaveAttribute("href", "/acme/issues/issue-1#comment-comment-1");
  });

  it("keeps a generic failure out of the conflict notices", async () => {
    outcome = async () => { throw new ApiError("boom", 500, "Server Error"); };
    renderCard(card());
    fireEvent.click(screen.getByTestId("decision-dm-disclaim"));
    fireEvent.click(await screen.findByRole("button", { name: "Disclaim" }));
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(screen.queryByTestId("decision-dm-conflict")).toBeNull();
    expect(await screen.findByTestId("decision-dm-error")).toBeInTheDocument();
    expect(screen.getByTestId("decision-dm-disclaim")).toBeEnabled();
  });
});
