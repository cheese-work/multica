import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderWithI18n } from "../../test/i18n";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
import { DecisionRequestCard } from "./decision-request-card";
import { ChatAnnotationContext } from "./chat-annotation-context";

vi.mock("../../navigation", async (importActual) => ({
  ...(await importActual<typeof import("../../navigation")>()),
  AppLink: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => <a href={href} {...rest}>{children}</a>,
}));

const answerDecisionRequest = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/api", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/api")>()),
  api: { answerDecisionRequest },
}));

const card = {
  id: "request", issue_id: "issue", issue_identifier: "CHE-2088",
  source_comment_id: "source", ask: "Keep the **original ask**.",
  state: "open" as const, created_at: "2026-10-26T23:30:00Z",
};

describe("DecisionRequestCard accessibility", () => {
  it("makes only an open ask annotatable, with the agent reply label and marker slot", () => {
    const view = (state: "open" | "answered" | "withdrawn") => (
      <ChatAnnotationContext.Provider value={{ enabled: true, agentName: "Terra" }}>
        <QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme">
          <DecisionRequestCard card={{ ...card, state }} />
        </WorkspaceSlugProvider></QueryClientProvider>
      </ChatAnnotationContext.Provider>
    );
    const { container, rerender } = renderWithI18n(view("open"));
    const ask = container.querySelector('[data-comment-content="decision-request:request"]');
    expect(ask).not.toBeNull();
    expect(ask).toHaveAttribute("tabindex", "0");
    expect(ask).toHaveAccessibleName("Message from Terra");
    expect(ask).toHaveTextContent("Keep the original ask.");
    expect(ask?.parentElement?.querySelector("[data-annotation-overlay-slot]")).not.toBeNull();
    for (const state of ["answered", "withdrawn"] as const) {
      rerender(view(state));
      expect(container.querySelector("[data-comment-content]")).toBeNull();
      expect(screen.getByText(/original ask/)).toBeInTheDocument();
    }
  });

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

  it("links an answered card to the issue comment that recorded it", () => {
    renderWithI18n(<QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={{ ...card, state: "answered", record_comment_id: "record" }} /></WorkspaceSlugProvider></QueryClientProvider>);
    const link = screen.getByRole("link", { name: "Recorded on CHE-2088" });
    expect(link).toHaveAttribute("href", expect.stringMatching(/\/issue#comment-record$/));
    expect(link).toHaveAttribute("data-testid", "decision-request-record");
  });

  it("shows no record link on an open card or an answered card without a record", () => {
    for (const next of [{ ...card, record_comment_id: "record" }, { ...card, state: "answered" as const }]) {
      const view = renderWithI18n(<QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={next} /></WorkspaceSlugProvider></QueryClientProvider>);
      expect(screen.queryByTestId("decision-request-record")).toBeNull();
      view.unmount();
    }
  });

  it("keeps a localized status label and tolerates missing legacy source and invalid time", () => {
    renderWithI18n(<QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={{ ...card, state: "withdrawn", source_comment_id: undefined, created_at: "invalid", reason: "Scope changed." }} /></WorkspaceSlugProvider></QueryClientProvider>, { locale: "zh-Hans" });
    expect(screen.getByRole("region", { name: "决策请求：已撤回" })).toHaveTextContent("Scope changed.");
    expect(screen.getByText(/original ask/)).toBeInTheDocument();
    expect(screen.getAllByRole("link")).toHaveLength(1);
    expect(document.querySelector("time")).toBeNull();
  });
});

describe("DecisionRequestCard answering", () => {
  const withOptions: RequestCard = { ...card, options: [{ label: "Option A", what_happens: "A happens" }, { label: "Option B", what_happens: "B happens" }] };
  const renderCard = (value: RequestCard = withOptions) =>
    renderWithI18n(<QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={value} /></WorkspaceSlugProvider></QueryClientProvider>);
  beforeEach(() => {
    answerDecisionRequest.mockReset().mockResolvedValue(undefined);
  });

  it("sends the selected option on Send, and only once chosen", async () => {
    renderCard();
    const user = userEvent.setup();
    const send = screen.getByRole("button", { name: "Send" });
    expect(send).toBeDisabled();
    await user.click(screen.getByRole("radio", { name: /Option B/ }));
    expect(screen.getByRole("radio", { name: /Option B/ })).toBeChecked();
    await user.click(send);
    expect(answerDecisionRequest).toHaveBeenCalledWith("request", { option: "Option B", text: undefined });
  });

  it("sends on Enter and keeps Shift+Enter as a newline", async () => {
    renderCard();
    const user = userEvent.setup();
    const field = screen.getByRole("textbox", { name: "Or write an answer" });
    await user.type(field, "Ship C{Shift>}{Enter}{/Shift}later");
    expect(answerDecisionRequest).not.toHaveBeenCalled();
    expect(field).toHaveValue("Ship C\nlater");
    await user.keyboard("{Enter}");
    expect(answerDecisionRequest).toHaveBeenCalledWith("request", { option: undefined, text: "Ship C\nlater" });
  });

  it("has no answer form once the card is answered", () => {
    renderCard({ ...withOptions, state: "answered" });
    expect(screen.queryByTestId("decision-request-answer")).toBeNull();
  });

  it("shows an error when the send fails", async () => {
    answerDecisionRequest.mockRejectedValue(new Error("boom"));
    renderCard({ ...card });
    const user = userEvent.setup();
    await user.type(screen.getByRole("textbox", { name: "Or write an answer" }), "hi{Enter}");
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not send");
  });
});
