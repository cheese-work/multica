import { act, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@multica/core/api";
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

  it("shows Recorded, Delivered and Disclaimed in place from the answer receipt", () => {
    const answered = { ...card, state: "answered" as const, record_comment_id: "record" };
    const answer = (state: "recorded" | "delivered" | "disclaimed") => ({
      ...answered, state: state === "disclaimed" ? ("open" as const) : answered.state,
      answer: { state, chosen_option_index: 0, option_label: "Ship now", answered_at: "2026-10-26T23:31:00Z" },
    });
    for (const [state, label] of [["recorded", "Recorded"], ["delivered", "Delivered"], ["disclaimed", "Disclaimed"]] as const) {
      const view = renderWithI18n(<QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={answer(state)} /></WorkspaceSlugProvider></QueryClientProvider>);
      expect(screen.getByTestId("decision-request-card")).toHaveAttribute("data-state", state);
      expect(screen.getByRole("region", { name: `Decision request: ${label}` })).toBeInTheDocument();
      view.unmount();
    }
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

  it("explains where Send posts and the keyboard contract, with a visible field label", () => {
    renderCard();
    expect(screen.getByText("Send posts your answer to CHE-2088. Questions for the DM agent go in the message box below.")).toBeVisible();
    expect(screen.getByText("Enter sends · Shift+Enter for a new line")).toBeVisible();
    expect(screen.getByRole("radiogroup", { name: "Options" })).toBeInTheDocument();
    expect(screen.getByLabelText("Or write an answer")).toHaveAccessibleDescription("Enter sends · Shift+Enter for a new line");
  });

  it("keeps the answer and disables Send while offline", async () => {
    const online = vi.spyOn(window.navigator, "onLine", "get").mockReturnValue(false);
    try {
      renderCard();
      const user = userEvent.setup();
      const field = screen.getByRole("textbox", { name: "Or write an answer" });
      await user.type(field, "Ship C{Enter}");
      expect(field).toHaveValue("Ship C");
      expect(screen.getByRole("button", { name: "Send" })).toBeDisabled();
      expect(screen.getByRole("status")).toHaveTextContent("You are offline. Your answer is kept.");
      expect(answerDecisionRequest).not.toHaveBeenCalled();
    } finally {
      online.mockRestore();
    }
  });

  it("keeps an unsent draft when the card turns answered from elsewhere", async () => {
    const client = new QueryClient();
    const view = (value: RequestCard) => (
      <QueryClientProvider client={client}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={value} /></WorkspaceSlugProvider></QueryClientProvider>
    );
    const { rerender } = renderWithI18n(view(withOptions));
    const user = userEvent.setup();
    await user.click(screen.getByRole("radio", { name: /Option B/ }));
    await user.type(screen.getByRole("textbox", { name: "Or write an answer" }), "my note");
    rerender(view({ ...withOptions, state: "answered", record_comment_id: "record" }));
    expect(screen.getByTestId("decision-request-record")).toBeInTheDocument();
    const kept = screen.getByTestId("decision-request-kept-draft");
    expect(kept).toHaveTextContent("Option B");
    expect(kept).toHaveTextContent("my note");
    expect(answerDecisionRequest).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Discard draft" }));
    expect(screen.queryByTestId("decision-request-kept-draft")).toBeNull();
  });

  it("shows no kept draft after a successful send turns the card answered", async () => {
    const client = new QueryClient();
    const view = (value: RequestCard) => (
      <QueryClientProvider client={client}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={value} /></WorkspaceSlugProvider></QueryClientProvider>
    );
    const { rerender } = renderWithI18n(view(withOptions));
    const user = userEvent.setup();
    await user.type(screen.getByRole("textbox", { name: "Or write an answer" }), "Ship C{Enter}");
    await vi.waitFor(() => expect(answerDecisionRequest).toHaveBeenCalled());
    rerender(view({ ...withOptions, state: "answered" }));
    expect(screen.queryByTestId("decision-request-kept-draft")).toBeNull();
  });

  describe("a send whose answered event and HTTP response arrive in either order", () => {
    const setup = () => {
      let resolveAnswer: (value?: unknown) => void = () => { throw new Error("response not initialized"); };
      answerDecisionRequest.mockReturnValue(new Promise((resolve) => { resolveAnswer = resolve; }));
      const client = new QueryClient();
      const view = (value: RequestCard) => (
        <QueryClientProvider client={client}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={value} /></WorkspaceSlugProvider></QueryClientProvider>
      );
      return { ...renderWithI18n(view(withOptions)), view, resolve: () => act(async () => { resolveAnswer(); await Promise.resolve(); }) };
    };

    it("event first, then 201: never shows the unposted-draft warning", async () => {
      const { rerender, view, resolve } = setup();
      const user = userEvent.setup();
      await user.type(screen.getByRole("textbox", { name: "Or write an answer" }), "Ship C{Enter}");
      await vi.waitFor(() => expect(answerDecisionRequest).toHaveBeenCalledTimes(1));
      rerender(view({ ...withOptions, state: "answered", record_comment_id: "own" }));
      expect(screen.queryByTestId("decision-request-kept-draft")).toBeNull();
      await resolve();
      await vi.waitFor(() => expect(screen.queryByTestId("decision-request-kept-draft")).toBeNull());
      expect(screen.queryByTestId("decision-request-answer")).toBeNull();
    });

    it("201 first, then event: clears the draft and shows no warning", async () => {
      const { rerender, view, resolve } = setup();
      const user = userEvent.setup();
      await user.type(screen.getByRole("textbox", { name: "Or write an answer" }), "Ship C{Enter}");
      await vi.waitFor(() => expect(answerDecisionRequest).toHaveBeenCalledTimes(1));
      await resolve();
      expect(screen.queryByTestId("decision-request-kept-draft")).toBeNull();
      rerender(view({ ...withOptions, state: "answered", record_comment_id: "own" }));
      expect(screen.queryByTestId("decision-request-kept-draft")).toBeNull();
    });

    it("event first, then 409: shows the refused draft once the send settles", async () => {
      let rejectAnswer: (error: unknown) => void = () => { throw new Error("response not initialized"); };
      answerDecisionRequest.mockReturnValue(new Promise((_, reject) => { rejectAnswer = reject; }));
      const client = new QueryClient();
      const view = (value: RequestCard) => (
        <QueryClientProvider client={client}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={value} /></WorkspaceSlugProvider></QueryClientProvider>
      );
      const { rerender } = renderWithI18n(view(withOptions));
      const user = userEvent.setup();
      await user.type(screen.getByRole("textbox", { name: "Or write an answer" }), "Ship C{Enter}");
      await vi.waitFor(() => expect(answerDecisionRequest).toHaveBeenCalledTimes(1));
      rerender(view({ ...withOptions, state: "answered" }));
      await act(async () => { rejectAnswer(new ApiError("answered first", 409, "Conflict")); await Promise.resolve(); });
      expect(await screen.findByTestId("decision-request-kept-draft")).toHaveTextContent("Ship C");
    });
  });

  it("shows the draft back with Discard draft when the server refuses with a conflict", async () => {
    answerDecisionRequest.mockRejectedValue(new ApiError("this request cannot accept another answer", 409, "Conflict"));
    renderCard();
    const user = userEvent.setup();
    await user.click(screen.getByRole("radio", { name: /Option A/ }));
    await user.type(screen.getByRole("textbox", { name: "Or write an answer" }), "with a note{Enter}");
    const kept = await screen.findByTestId("decision-request-kept-draft");
    expect(kept).toHaveTextContent("Nothing from your draft was posted.");
    expect(kept).toHaveTextContent("Option A");
    expect(kept).toHaveTextContent("with a note");
    expect(screen.queryByTestId("decision-request-answer")).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Discard draft" }));
    expect(screen.queryByTestId("decision-request-kept-draft")).toBeNull();
  });
});
