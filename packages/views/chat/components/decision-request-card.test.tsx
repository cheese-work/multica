import { act, screen, waitFor } from "@testing-library/react";
import { forwardRef } from "react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@multica/core/api";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { chatKeys } from "@multica/core/chat/queries";
import { applyDecisionAnswerUpdated } from "@multica/core/realtime";
import { renderWithI18n } from "../../test/i18n";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
import { useDecisionAnswerDraftStore } from "@multica/core/issues/stores/decision-answer-draft-store";
import { DecisionRequestCard } from "./decision-request-card";
import { ChatAnnotationContext } from "./chat-annotation-context";

const ui = vi.hoisted(() => ({ desktop: false }));
const openExternal = vi.hoisted(() => vi.fn());
const challengeDecisionAnswer = vi.hoisted(() => vi.fn());
const signPasskeyChallenge = vi.hoisted(() => vi.fn());
vi.mock("../../platform", () => ({ openExternal }));
vi.mock("../../platform/local-directory", () => ({ isDesktopShell: () => ui.desktop }));
vi.mock("@multica/core/auth/webauthn", () => ({ signPasskeyChallenge }));

vi.mock("../../navigation", async (importActual) => ({
  ...(await importActual<typeof import("../../navigation")>()),
  useOptionalNavigation: () => ({ getShareableUrl: (path: string) => `https://app.test${path}` }),
  AppLink: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => <a href={href} {...rest}>{children}</a>,
}));

vi.mock("../../issues/components/issue-chip", () => ({
  IssueChip: ({ fallbackLabel, children }: { fallbackLabel?: string; children?: React.ReactNode }) => <span data-testid="issue-chip">{children ?? fallbackLabel}</span>,
}));

// Resolving a comment needs a workspace query; here only the link it falls back to matters.
vi.mock("../../issues/components/comment-mention-card", () => ({
  CommentMentionCard: ({ fallback }: { fallback: React.ReactNode }) => <>{fallback}</>,
}));

const answerDecisionRequest = vi.hoisted(() => vi.fn());
const listDecisionArtifacts = vi.hoisted(() => vi.fn());
const uploadFile = vi.hoisted(() => vi.fn());
const getDecisionActionOutcome = vi.hoisted(() => vi.fn());
const challengeDecisionAction = vi.hoisted(() => vi.fn());
const approveDecisionAction = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/api", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/api")>()),
  api: { answerDecisionRequest, challengeDecisionAnswer, listDecisionArtifacts, uploadFile, getDecisionActionOutcome, challengeDecisionAction, approveDecisionAction },
}));

// Config-change approval is owner-only: the role comes from the member list of the current workspace.
const member = vi.hoisted(() => ({ role: "owner" as string | null, workspace: { id: "ws-1" } as { id: string } | null }));
vi.mock("@multica/core/permissions", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/permissions")>()),
  useCurrentMember: () => ({ userId: "u1", role: member.role, member: null, isLoading: member.role === null }),
}));
vi.mock("@multica/core/paths", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/paths")>()),
  useCurrentWorkspace: () => member.workspace,
}));

// The real editor is Tiptap; its own suites cover typing and paste. Here it is
// a textarea that exposes the props the card wires up.
type EditorProps = {
  value?: string;
  defaultValue?: string;
  ariaLabel?: string;
  ariaDescribedBy?: string;
  onUpdate?: (markdown: string) => void;
  onSubmit?: () => void;
  onUploadFile?: (file: File, uploadId: string) => Promise<unknown>;
  onUploadingChange?: (uploading: boolean) => void;
};
// `live` is what the editor holds right now; the card only hears of it through onUpdate, after a timer.
const editor = vi.hoisted(() => ({ props: undefined as unknown, hasActiveUploads: false, live: undefined as string | undefined }));
const editorProps = () => editor.props as EditorProps;
vi.mock("./lazy-content-editor", () => ({
  LazyContentEditor: forwardRef(function MockEditor(props: EditorProps, ref) {
    editor.props = props;
    const handle = { hasActiveUploads: () => editor.hasActiveUploads, getMarkdown: () => editor.live ?? props.defaultValue ?? "" };
    if (typeof ref === "function") ref(handle as never);
    else if (ref) (ref as { current: unknown }).current = handle;
    return (
      <textarea
        aria-label={props.ariaLabel}
        aria-describedby={props.ariaDescribedBy}
        defaultValue={props.defaultValue ?? ""}
        onChange={(event) => props.onUpdate?.(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && !event.shiftKey) {
            event.preventDefault();
            props.onSubmit?.();
          }
        }}
      />
    );
  }),
}));

const card = {
  id: "request", issue_id: "issue", issue_identifier: "CHE-2088",
  source_comment_id: "source", ask: "Keep the **original ask**.",
  state: "open" as const, created_at: "2026-10-26T23:30:00Z",
};

describe("DecisionRequestCard accessibility", () => {
  beforeEach(() => listDecisionArtifacts.mockResolvedValue([]));
  afterEach(() => vi.useRealTimers());

  it("puts 'You are approving' before the ask, and renders as today without files", async () => {
    const view = () => (
      <QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={card} /></WorkspaceSlugProvider></QueryClientProvider>
    );
    const { container, unmount } = renderWithI18n(view());
    await waitFor(() => expect(listDecisionArtifacts).toHaveBeenCalledWith("request"));
    expect(screen.queryByRole("region", { name: "You are approving" })).toBeNull();
    expect(container.querySelector('[data-testid="decision-request-artifacts"]')).toBeNull();
    unmount();
    listDecisionArtifacts.mockResolvedValue([{
      attachment_id: "att", label: "Design revision 5", sha256: "3f9a2c1".padEnd(64, "0"),
      attachment: { id: "att", filename: "design-rev5.md", content_type: "text/markdown", size_bytes: 10, download_url: "https://files.test/att" },
    }]);
    renderWithI18n(view());
    const section = await screen.findByRole("region", { name: "You are approving" });
    const ask = screen.getByTestId("decision-request-ask");
    expect(section.compareDocumentPosition(ask) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

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

  it("names the asking agent in the header and omits it when the name is missing", () => {
    const view = renderWithI18n(<QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={{ ...card, asking_agent_name: "x99-claude-sonnet" }} /></WorkspaceSlugProvider></QueryClientProvider>);
    expect(screen.getByTestId("decision-request-asker")).toHaveTextContent("from x99-claude-sonnet");
    view.unmount();
    renderWithI18n(<QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={card} /></WorkspaceSlugProvider></QueryClientProvider>);
    expect(screen.queryByTestId("decision-request-asker")).toBeNull();
  });

  it("links an answered card to the issue comment that recorded it", () => {
    renderWithI18n(<QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={{ ...card, state: "answered", record_comment_id: "record" }} /></WorkspaceSlugProvider></QueryClientProvider>);
    const link = screen.getByRole("link", { name: "Recorded on CHE-2088" });
    expect(link).toHaveAttribute("href", expect.stringMatching(/\/issue#comment-record$/));
    expect(link).toHaveAttribute("data-testid", "decision-request-record");
  });

  it("shows Recorded, Delivered and Disclaimed in place from the answer receipt", () => {
    vi.useFakeTimers({ now: new Date("2026-10-26T23:31:30Z"), toFake: ["Date", "setTimeout", "clearTimeout"] });
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

  describe("pickup after the record", () => {
    const answered = (answer: Partial<NonNullable<RequestCard["answer"]>> = {}) => ({
      ...card, state: "answered" as const, record_comment_id: "record",
      answer: { state: "delivered" as const, answered_at: "2026-10-26T23:31:00Z", ...answer },
    });
    const view = (next: RequestCard) => renderWithI18n(<QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={next} /></WorkspaceSlugProvider></QueryClientProvider>);
    const at = (iso: string) => vi.useFakeTimers({ now: new Date(iso), toFake: ["Date", "setTimeout", "clearTimeout"] });

    it("shows Picked up with the agent and a link to that run once a run exists", () => {
      at("2026-10-26T23:31:30Z");
      view(answered({ state: "picked_up", run: { id: "task-1", agent_name: "Sol" } }));
      expect(screen.getByTestId("decision-request-card")).toHaveAttribute("data-state", "picked_up");
      expect(screen.getByRole("region", { name: "Decision request: Picked up" })).toBeInTheDocument();
      const link = screen.getByTestId("decision-request-run");
      expect(link).toHaveTextContent("Picked up by Sol");
      expect(link).toHaveAttribute("href", expect.stringMatching(/\/issue\?run=task-1$/));
    });

    it("moves from Recorded to Not picked up once 2 minutes pass with no run", () => {
      at("2026-10-26T23:32:00Z");
      view(answered());
      expect(screen.getByTestId("decision-request-card")).toHaveAttribute("data-state", "delivered");
      expect(screen.queryByTestId("decision-request-not-picked-up")).toBeNull();
      act(() => { vi.advanceTimersByTime(59_000); });
      expect(screen.getByTestId("decision-request-card")).toHaveAttribute("data-state", "delivered");
      act(() => { vi.advanceTimersByTime(1_000); });
      expect(screen.getByRole("region", { name: "Decision request: Not picked up" })).toBeInTheDocument();
      expect(screen.getByTestId("decision-request-not-picked-up")).toHaveAttribute("href", expect.stringMatching(/\/issue$/));
    });

    it("shows Not picked up at once for an old answer with no run, but never for a disclaimed one", () => {
      at("2026-10-27T00:00:00Z");
      const { unmount } = view(answered({ state: "recorded" }));
      expect(screen.getByTestId("decision-request-card")).toHaveAttribute("data-state", "not_picked_up");
      unmount();
      view({ ...answered({ state: "disclaimed" }), state: "open" });
      expect(screen.getByTestId("decision-request-card")).toHaveAttribute("data-state", "disclaimed");
      expect(screen.queryByTestId("decision-request-not-picked-up")).toBeNull();
    });

    it("shows Not picked up when an old answered receipt arrives on an already-mounted card", () => {
      at("2026-10-26T23:31:00Z");
      const client = new QueryClient();
      const tree = (next: RequestCard) => <QueryClientProvider client={client}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={next} /></WorkspaceSlugProvider></QueryClientProvider>;
      const { rerender } = renderWithI18n(tree(card));
      act(() => { vi.advanceTimersByTime(3 * 60_000); });
      rerender(tree(answered()));
      expect(screen.getByTestId("decision-request-card")).toHaveAttribute("data-state", "not_picked_up");
    });
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

  // Copied from the golden output of decisiontx Ask.Content() (ask_test.go).
  const goldenAsk = "**Context**\nA change is ready. You decide when it ships.\n\n**Decision**\nShip now?\n\n**What is at stake**\nA bad change could reach users.\n\n**Options**\n- **NOW**: NOW happens.\n  - Pro: Pro\n  - Con: Con\n  - Cost or time: No extra time\n- **LATER**: LATER happens.\n  - Pro: Pro\n  - Con: Con\n  - Cost or time: No extra time\n\n**Recommendation**\nLATER\n\n**Default if no reply**\nLATER\n\n<details>\n<summary>Details</summary>\n\n**Agent did**\nRan all requested checks.\n\n</details>\n\nInvestigate in issue: [Issue](mention://issue/11111111-1111-4111-8111-111111111111) · [Comment](/issue/11111111-1111-4111-8111-111111111111#comment-22222222-2222-4222-8222-222222222222)";

  const renderCard = (ask: string) => renderWithI18n(<QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={{ ...card, ask }} /></WorkspaceSlugProvider></QueryClientProvider>);

  it("renders the ELI10 ask in order with Details collapsed", () => {
    const { container } = renderCard(goldenAsk);
    const text = container.textContent ?? "";
    const marks = ["Context", "Options", "Recommendation", "Default if no reply", "Details", "Ran all requested checks.", "Investigate in issue"].map((mark) => text.indexOf(mark));
    expect(marks.every((at, index) => at >= 0 && at > (marks[index - 1] ?? -1))).toBe(true);
    const details = container.querySelector("details");
    expect(details).not.toBeNull();
    expect(details).not.toHaveAttribute("open");
    expect(details?.querySelector("summary")).toHaveTextContent("Details");
    expect(details).toHaveTextContent("Ran all requested checks.");
    expect(details).not.toHaveTextContent("Investigate in issue");
    expect(text.match(/Investigate in issue/g)).toHaveLength(1);
  });

  // CHE-3411: the link shape the server emits today (`util.WorkspaceIssuePath`):
  // the Issue chip carries the UUID, the Comment link is a workspace UUID path.
  it("links the Issue chip and the Comment link to the issue by UUID path", () => {
    const issueId = "01a1a0eb-718d-778e-9948-770bc5e04ad7";
    const commentId = "01a1a165-1b88-769c-8137-bd8be641b61b";
    const ask = `Ship?\n\nInvestigate in issue: [Issue](mention://issue/${issueId}) · [Comment](/acme/issues/${issueId}#comment-${commentId})`;
    const { container } = renderCard(ask);
    const hrefs = Array.from(container.querySelectorAll("a")).map((a) => a.getAttribute("href"));
    expect(hrefs).toContain(`/acme/issues/${issueId}`);
    expect(hrefs).toContain(`/acme/issues/${issueId}#comment-${commentId}`);
    expect(hrefs.join(" ")).not.toContain("mention://");
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

describe("DecisionRequestCard answering", () => {
  const withOptions: RequestCard = {
    ...card,
    options: [
      { label: "Option A", what_happens: "A happens", pros: ["fast"], cons: ["risky"], cost_or_time: "1 h" },
      { label: "Option B", what_happens: "B happens", pros: ["safe"], cons: ["slow"], cost_or_time: "2 days" },
    ],
    recommended_option_index: 1,
  };
  const renderCard = (value: RequestCard = withOptions) =>
    renderWithI18n(<QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={value} /></WorkspaceSlugProvider></QueryClientProvider>);
  beforeEach(() => {
    useDecisionAnswerDraftStore.setState({ drafts: {} });
    answerDecisionRequest.mockReset().mockResolvedValue(undefined);
    uploadFile.mockReset();
    editor.hasActiveUploads = false;
    editor.live = undefined;
  });

  it("never feeds its draft back into the editor, so a late echo cannot overwrite newer typing", async () => {
    renderCard();
    expect(editorProps()).not.toHaveProperty("value");
    await userEvent.setup().type(screen.getByRole("textbox", { name: "Add a note (optional)" }), "before ");
    expect(editorProps()).not.toHaveProperty("value");
  });

  it("empties the editor after a successful send while the card is still open", async () => {
    renderCard({ ...card, options: undefined });
    const user = userEvent.setup();
    const box = screen.getByRole("textbox", { name: "Write an answer" });
    await user.type(box, "Ship it");
    editor.live = "Ship it";
    await user.click(screen.getByRole("button", { name: "Send" }));
    await vi.waitFor(() => expect(answerDecisionRequest).toHaveBeenCalledTimes(1));
    await vi.waitFor(() => expect(screen.getByRole("textbox", { name: "Write an answer" })).toHaveValue(""));
  });

  it("selects an option on tap without sending, and Send posts the chosen option", async () => {
    renderCard();
    const user = userEvent.setup();
    expect(screen.getByRole("button", { name: "Send" })).toBeDisabled();
    const optionB = screen.getByRole("radio", { name: /Option B/ });
    expect(optionB).toHaveAttribute("aria-checked", "false");
    await user.click(optionB);
    expect(answerDecisionRequest).not.toHaveBeenCalled();
    expect(optionB).toHaveAttribute("aria-checked", "true");
    expect(screen.getByRole("radio", { name: /Option A/ })).toHaveAttribute("aria-checked", "false");
    await user.click(screen.getByRole("button", { name: "Send" }));
    expect(answerDecisionRequest).toHaveBeenCalledTimes(1);
    expect(answerDecisionRequest).toHaveBeenCalledWith("request", { option: "Option B", text: undefined });
  });

  it("acknowledges the covered cards when a batch card's option is chosen", async () => {
    const batchId = "00000000-0000-4000-8000-0000000000b1";
    renderCard({ ...withOptions, id: batchId, covers_request_ids: ["00000000-0000-4000-8000-0000000000c2", "00000000-0000-4000-8000-0000000000c1"],
      covered_cards: ["c2", "c1"].map((tail) => ({ id: `00000000-0000-4000-8000-0000000000${tail}`, issue_id: `issue-${tail}`, issue_identifier: `CHE-${tail}`, decision: "Same ask?", options: withOptions.options })),
    });
    const user = userEvent.setup();
    await user.click(screen.getByRole("radio", { name: /Option B/ }));
    await user.click(screen.getByRole("button", { name: "Send" }));
    await vi.waitFor(() => expect(answerDecisionRequest).toHaveBeenCalledTimes(1));
    // Same vector as the server test: the digest of the batch id, the sorted covered ids and the option.
    expect(answerDecisionRequest).toHaveBeenCalledWith(batchId, {
      option: "Option B", text: undefined,
      covers_ack: "012ee7331cfe77d20ca96126e0cae16df3acabee3149e329bb3054f42d0211e1",
    });
  });

  it("sends no acknowledgment for a direct answer", async () => {
    renderCard();
    const user = userEvent.setup();
    await user.click(screen.getByRole("radio", { name: /Option B/ }));
    await user.click(screen.getByRole("button", { name: "Send" }));
    await vi.waitFor(() => expect(answerDecisionRequest).toHaveBeenCalledTimes(1));
    expect(answerDecisionRequest.mock.calls[0]?.[1]).not.toHaveProperty("covers_ack");
  });

  it("does not style an unselected recommended option as selected; the badge is its only cue", async () => {
    renderCard();
    const optionB = screen.getByRole("radio", { name: /Option B/ });
    expect(optionB).toHaveTextContent("Recommended");
    expect(optionB).toHaveAttribute("aria-checked", "false");
    expect(optionB.className).not.toMatch(/border-primary|bg-primary|ring-primary/);
    await userEvent.setup().click(optionB);
    expect(optionB.className).toMatch(/border-primary/);
    expect(optionB.className).toMatch(/ring-primary/);
  });

  it("gives Send and Clear selection a 44px touch height on a phone", async () => {
    renderCard();
    await userEvent.setup().click(screen.getByRole("radio", { name: /Option A/ }));
    for (const name of ["Send", "Clear selection"]) {
      expect(screen.getByRole("button", { name }).className).toContain("max-sm:h-11");
    }
  });

  it("deselects on a second tap or on Clear, and Send is disabled again", async () => {
    renderCard();
    const user = userEvent.setup();
    const optionA = screen.getByRole("radio", { name: /Option A/ });
    await user.click(optionA);
    await user.click(optionA);
    expect(optionA).toHaveAttribute("aria-checked", "false");
    expect(screen.getByRole("button", { name: "Send" })).toBeDisabled();
    expect(screen.queryByRole("button", { name: "Clear selection" })).not.toBeInTheDocument();
    await user.click(optionA);
    await user.click(screen.getByRole("button", { name: "Clear selection" }));
    expect(optionA).toHaveAttribute("aria-checked", "false");
    expect(screen.getByRole("button", { name: "Send" })).toBeDisabled();
    expect(answerDecisionRequest).not.toHaveBeenCalled();
  });

  it("keeps the selection and the note across an unmount until it is sent", async () => {
    const first = renderCard();
    const user = userEvent.setup();
    await user.click(screen.getByRole("radio", { name: /Option A/ }));
    await user.type(screen.getByRole("textbox", { name: "Add a note (optional)" }), "later");
    first.unmount();
    renderCard();
    expect(screen.getByRole("radio", { name: /Option A/ })).toHaveAttribute("aria-checked", "true");
    expect(screen.getByRole("textbox", { name: "Add a note (optional)" })).toHaveValue("later");
    editor.live = "later";
    await user.click(screen.getByRole("button", { name: "Send" }));
    await vi.waitFor(() => expect(answerDecisionRequest).toHaveBeenCalledWith("request", { option: "Option A", text: "later" }));
    await vi.waitFor(() => expect(useDecisionAnswerDraftStore.getState().getDraft("request")).toBeUndefined());
  });

  it("shows each option's pros, cons and cost, and marks the recommendation", () => {
    renderCard();
    const second = screen.getByRole("radio", { name: /Option B/ });
    expect(second).toHaveTextContent("safe");
    expect(second).toHaveTextContent("slow");
    expect(second).toHaveTextContent("2 days");
    expect(second).toHaveTextContent("Recommended");
    const first = screen.getByRole("radio", { name: /Option A/ });
    expect(first).toHaveTextContent("fast");
    expect(first).not.toHaveTextContent("Recommended");
  });

  it("sends on Enter and keeps Shift+Enter as a newline", async () => {
    renderCard();
    const user = userEvent.setup();
    const field = screen.getByRole("textbox", { name: "Add a note (optional)" });
    await user.type(field, "Ship C{Shift>}{Enter}{/Shift}later");
    expect(answerDecisionRequest).not.toHaveBeenCalled();
    expect(field).toHaveValue("Ship C\nlater");
    await user.keyboard("{Enter}");
    expect(answerDecisionRequest).toHaveBeenCalledWith("request", { option: undefined, text: "Ship C\nlater" });
  });

  it("labels the text field a note beside options, and an answer when the card has none", () => {
    const { unmount } = renderCard();
    expect(screen.getByRole("textbox", { name: "Add a note (optional)" })).toBeInTheDocument();
    expect(screen.queryByText("Or write an answer")).toBeNull();
    unmount();
    renderCard({ ...card });
    expect(screen.getByRole("textbox", { name: "Write an answer" })).toBeInTheDocument();
  });

  it("sends the option and the note together", async () => {
    renderCard();
    const user = userEvent.setup();
    await user.type(screen.getByRole("textbox", { name: "Add a note (optional)" }), "but wait a day");
    editor.live = "but wait a day";
    await user.click(screen.getByRole("radio", { name: /Option A/ }));
    expect(answerDecisionRequest).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Send" }));
    expect(answerDecisionRequest).toHaveBeenCalledWith("request", { option: "Option A", text: "but wait a day" });
  });

  describe("image paste", () => {
    it("uploads a pasted file against the card's issue and returns it to the editor", async () => {
      uploadFile.mockResolvedValue({ id: "att-1", url: "https://cdn/x.png", markdown_url: "https://app/att-1" });
      renderCard();
      const file = new File(["png"], "x.png", { type: "image/png" });
      const result = await editorProps().onUploadFile?.(file, "upload-1");
      expect(uploadFile).toHaveBeenCalledWith(file, { issueId: "issue", commentId: undefined, chatSessionId: undefined }, expect.any(AbortSignal));
      expect(result).toMatchObject({ markdownLink: "https://app/att-1" });
    });

    it("sends the image markdown the editor produced as the answer text", async () => {
      renderCard();
      const user = userEvent.setup();
      act(() => editorProps().onUpdate?.("see ![x.png](https://app/att-1)"));
      await user.click(screen.getByRole("button", { name: "Send" }));
      expect(answerDecisionRequest).toHaveBeenCalledWith("request", { option: undefined, text: "see ![x.png](https://app/att-1)" });
    });

    it("sends what the editor holds when Send comes before its pending update fires", async () => {
      renderCard();
      const user = userEvent.setup();
      act(() => editorProps().onUpdate?.("stale"));
      editor.live = "stale and the last edit ![x.png](https://app/att-2)";
      await user.click(screen.getByRole("button", { name: "Send" }));
      expect(answerDecisionRequest).toHaveBeenCalledWith("request", { option: undefined, text: "stale and the last edit ![x.png](https://app/att-2)" });
    });

    it("sends on Enter typed before the card has heard of any text", async () => {
      renderCard();
      const user = userEvent.setup();
      editor.live = "typed just now";
      await user.type(screen.getByRole("textbox", { name: "Add a note (optional)" }), "{Enter}");
      expect(answerDecisionRequest).toHaveBeenCalledWith("request", { option: undefined, text: "typed just now" });
    });

    it("holds Send and Enter while an upload is in flight", async () => {
      renderCard();
      const user = userEvent.setup();
      await user.type(screen.getByRole("textbox", { name: "Add a note (optional)" }), "hi");
      act(() => editorProps().onUploadingChange?.(true));
      expect(screen.getByRole("button", { name: "Send" })).toBeDisabled();
      editor.hasActiveUploads = true;
      await user.keyboard("{Enter}");
      expect(answerDecisionRequest).not.toHaveBeenCalled();
      act(() => editorProps().onUploadingChange?.(false));
      editor.hasActiveUploads = false;
      expect(screen.getByRole("button", { name: "Send" })).toBeEnabled();
    });
  });

  describe("the 8 KB answer cap", () => {
    it("keeps an answer of exactly 8 KB sendable", async () => {
      renderCard();
      act(() => editorProps().onUpdate?.("x".repeat(8 << 10)));
      expect(screen.getByRole("button", { name: "Send" })).toBeEnabled();
      expect(screen.queryByText(/over 8 KB/)).toBeNull();
    });

    it("does not send an over-8 KB edit the card has not heard of yet", async () => {
      renderCard();
      const user = userEvent.setup();
      act(() => editorProps().onUpdate?.("short"));
      editor.live = "x".repeat((8 << 10) + 1);
      await user.click(screen.getByRole("button", { name: "Send" }));
      expect(answerDecisionRequest).not.toHaveBeenCalled();
    });

    it("blocks Send and says why once the text is over 8 KB, counting bytes not characters", async () => {
      renderCard();
      act(() => editorProps().onUpdate?.("é".repeat((8 << 10) / 2 + 1)));
      expect(screen.getByRole("button", { name: "Send" })).toBeDisabled();
      expect(screen.getByRole("alert")).toHaveTextContent("over 8 KB");
      const user = userEvent.setup();
      await user.type(screen.getByRole("textbox", { name: "Add a note (optional)" }), "{Enter}");
      expect(answerDecisionRequest).not.toHaveBeenCalled();
    });
  });

  it("has no answer form once the card is answered", () => {
    renderCard({ ...withOptions, state: "answered" });
    expect(screen.queryByTestId("decision-request-answer")).toBeNull();
  });

  it("shows an error when the send fails", async () => {
    answerDecisionRequest.mockRejectedValue(new Error("boom"));
    renderCard({ ...card });
    const user = userEvent.setup();
    await user.type(screen.getByRole("textbox", { name: "Write an answer" }), "hi{Enter}");
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not send");
  });

  it("explains where Send posts and the keyboard contract, with a visible field label", () => {
    renderCard();
    expect(screen.getByText("Send posts your answer to CHE-2088. Questions for the DM agent go in the message box below.")).toBeVisible();
    // The editor sends on the configured shortcut (Ctrl/Cmd+Enter by default), so the hint names that key.
    expect(screen.getByText(/^(Ctrl\+Enter|⌘↵) sends$/)).toBeVisible();
    expect(screen.getByRole("radiogroup", { name: "Options" })).toBeInTheDocument();
    expect(screen.getByLabelText("Add a note (optional)")).toHaveAccessibleDescription(/^(Ctrl\+Enter|⌘↵) sends$/);
  });

  it("keeps the answer and disables Send while offline", async () => {
    const online = vi.spyOn(window.navigator, "onLine", "get").mockReturnValue(false);
    try {
      renderCard();
      const user = userEvent.setup();
      const field = screen.getByRole("textbox", { name: "Add a note (optional)" });
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
    await user.type(screen.getByRole("textbox", { name: "Add a note (optional)" }), "my note");
    rerender(view({ ...withOptions, state: "answered", record_comment_id: "record" }));
    expect(screen.getByTestId("decision-request-record")).toBeInTheDocument();
    const kept = screen.getByTestId("decision-request-kept-draft");
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
    await user.type(screen.getByRole("textbox", { name: "Add a note (optional)" }), "Ship C{Enter}");
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
      await user.type(screen.getByRole("textbox", { name: "Add a note (optional)" }), "Ship C{Enter}");
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
      await user.type(screen.getByRole("textbox", { name: "Add a note (optional)" }), "Ship C{Enter}");
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
      await user.type(screen.getByRole("textbox", { name: "Add a note (optional)" }), "Ship C{Enter}");
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
    await user.type(screen.getByRole("textbox", { name: "Add a note (optional)" }), "with a note");
    editor.live = "with a note";
    await user.click(screen.getByRole("radio", { name: /Option A/ }));
    await user.click(screen.getByRole("button", { name: "Send" }));
    const kept = await screen.findByTestId("decision-request-kept-draft");
    expect(kept).toHaveTextContent("Nothing from your draft was posted.");
    expect(kept).toHaveTextContent("Option A");
    expect(kept).toHaveTextContent("with a note");
    expect(screen.queryByTestId("decision-request-answer")).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Discard draft" }));
    expect(screen.queryByTestId("decision-request-kept-draft")).toBeNull();
  });

  describe("action preview", () => {
    const preview = {
      fields: [
        { label: "Run cap", current: "10", proposed: "30" },
        { label: "Runs left", current: "0", proposed: "20" },
      ],
      sha256: "d".repeat(64),
    };
    const action = { id: "action", action_type: "issue.run_budget.raise", payload_sha256: "payload", preview };
    const show = (value: RequestCard["action"]) => renderCard({ ...card, action: value });
    beforeEach(() => getDecisionActionOutcome.mockReset().mockResolvedValue({ id: "action", state: "pending" }));

    it("shows the current and proposed values and enables Approve", async () => {
      show(action);
      const region = await screen.findByTestId("decision-request-action-preview");
      expect(region).toHaveTextContent("Run cap");
      expect(region).toHaveTextContent("10");
      expect(region).toHaveTextContent("30");
      expect(region).toHaveTextContent("Runs left");
      expect(await screen.findByRole("button", { name: "Approve with passkey" })).toBeEnabled();
      expect(screen.queryByText("Details unavailable")).toBeNull();
    });

    it.each([
      ["missing", undefined],
      ["empty", { fields: [], sha256: "d".repeat(64) }],
      ["digestless", { ...preview, sha256: "" }],
      ["not a sha-256 digest", { ...preview, sha256: "digest" }],
      ["partly malformed", { ...preview, fields: [{ label: "Run cap" }] } as unknown as typeof preview],
    ])("says details are unavailable, keeps Approve disabled and keeps Reject when the preview is %s", async (_name, bad) => {
      show({ ...action, preview: bad });
      expect(screen.getByText("Details unavailable")).toBeInTheDocument();
      expect(await screen.findByRole("button", { name: "Reject" })).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Approve with passkey" })).toBeDisabled();
    });

    it("keeps Approve disabled and Reject for an action type this client does not know, even with fields", async () => {
      show({ ...action, action_type: "future.thing" });
      expect(screen.getByText("Details unavailable")).toBeInTheDocument();
      expect(screen.queryByTestId("decision-request-action-preview")).toBeNull();
      expect(await screen.findByRole("button", { name: "Reject" })).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Approve with passkey" })).toBeDisabled();
    });

    describe("config change", () => {
      const change = {
        field: "workspace.context",
        target_id: "ws-1",
        keys: [] as string[],
        diff_kind: "text" as const,
        diff: [{ old_start: 1, new_start: 1, lines: ["-old rule", "+new rule"] }],
        after: "new rule",
        before_sha256: "b".repeat(64),
        after_sha256: "c".repeat(64),
        sha256: "e".repeat(64),
      };
      const configAction = { id: "action", action_type: "workspace.context.set", payload_sha256: "payload", config_change: change, owner_only: true };
      const approve = () => screen.findByRole("button", { name: "Approve with passkey" });
      // An approve that left this tab is remembered per action id for the session; every test starts clean.
      beforeEach(() => {
        sessionStorage.clear();
        member.role = "owner";
        member.workspace = { id: "ws-1" };
        challengeDecisionAction.mockReset().mockResolvedValue({ challenge_id: "ch" });
        signPasskeyChallenge.mockReset().mockResolvedValue({ id: "cred" });
        approveDecisionAction.mockReset().mockResolvedValue({ id: "action", state: "executed" });
      });

      afterEach(() => sessionStorage.clear());

      it("shows the server preview in the viewer and enables Approve for the owner", async () => {
        show(configAction);
        const viewer = await screen.findByTestId("config-change-viewer");
        expect(viewer).toHaveTextContent("Workspace context");
        await userEvent.setup().click(screen.getByTestId("config-change-tab-changes"));
        expect(screen.getByRole("region", { name: "Changes to Workspace context" })).toHaveTextContent("new rule");
        expect(await approve()).toBeEnabled();
        expect(screen.queryByText("Details unavailable")).toBeNull();
      });

      it("approves with the digest of the preview that was shown", async () => {
        show(configAction);
        await userEvent.setup().click(await approve());
        await waitFor(() => expect(approveDecisionAction).toHaveBeenCalledOnce());
        expect(approveDecisionAction).toHaveBeenCalledWith("action", { payload_sha256: "payload", assertion: { id: "cred" }, preview_sha256: change.sha256 });
      });

      it.each([
        ["a member", "member"],
        ["an admin", "admin"],
        ["a person whose role is not known yet", null],
      ])("hides Approve for %s and keeps Reject", async (_name, role) => {
        member.role = role;
        show(configAction);
        expect(await screen.findByRole("button", { name: "Reject" })).toBeInTheDocument();
        expect(screen.getByTestId("config-change-viewer")).toBeInTheDocument();
        expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
      });

      it("hides Approve when there is no current workspace to read the role from", async () => {
        member.workspace = null;
        show(configAction);
        expect(await screen.findByRole("button", { name: "Reject" })).toBeInTheDocument();
        expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
      });

      it("treats a config-change action as owner-only even when the server flag is missing", async () => {
        member.role = "member";
        show({ ...configAction, owner_only: undefined });
        expect(await screen.findByRole("button", { name: "Reject" })).toBeInTheDocument();
        expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
      });

      it.each([
        ["missing", undefined],
        ["not a Scope A field", { ...change, field: "members.roles" }],
        ["digestless", { ...change, sha256: "" }],
        ["a malformed diff line", { ...change, diff: [{ old_start: 1, new_start: 1, lines: ["?x"] }] }],
      ])("says details are unavailable and keeps Approve disabled for the owner, with Reject, when the change is %s", async (_name, bad) => {
        show({ ...configAction, config_change: bad });
        expect(screen.getByText("Details unavailable")).toBeInTheDocument();
        expect(screen.queryByTestId("config-change-viewer")).toBeNull();
        expect(await screen.findByRole("button", { name: "Reject" })).toBeInTheDocument();
        expect(screen.getByRole("button", { name: "Approve with passkey" })).toBeDisabled();
      });

      it.each([
        ["missing", undefined],
        ["not a Scope A field", { ...change, field: "members.roles" }],
        ["digestless", { ...change, sha256: "" }],
        ["a malformed diff line", { ...change, diff: [{ old_start: 1, new_start: 1, lines: ["?x"] }] }],
      ])("shows a non-owner no Approve and keeps Reject when the change is %s", async (_name, bad) => {
        member.role = "member";
        show({ ...configAction, config_change: bad });
        expect(screen.getByText("Details unavailable")).toBeInTheDocument();
        expect(await screen.findByRole("button", { name: "Reject" })).toBeInTheDocument();
        expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
      });

      it("shows no Approve and keeps Reject when there is no current workspace and the change is missing", async () => {
        member.workspace = null;
        show({ ...configAction, config_change: undefined });
        expect(screen.getByText("Details unavailable")).toBeInTheDocument();
        expect(await screen.findByRole("button", { name: "Reject" })).toBeInTheDocument();
        expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
      });

      it("shows the viewer for a settings change with its keys", async () => {
        show({ ...configAction, action_type: "workspace.settings.patch", config_change: { ...change, field: "workspace.settings", keys: ["compact_prompt"] } });
        expect(await screen.findByTestId("config-change-keys")).toHaveTextContent("compact_prompt");
      });

      it("keeps the run-budget preview and Approve for a person who is not the owner", async () => {
        member.role = "member";
        show(action);
        expect(await screen.findByTestId("decision-request-action-preview")).toBeInTheDocument();
        expect(await approve()).toBeEnabled();
        expect(screen.queryByTestId("config-change-viewer")).toBeNull();
      });
    });

    describe("agent note and outcome in the DM", () => {
      const note = "**Agent's note (not verified)**\n> Need more runs\n> [click](https://evil.test)\n> " + "x".repeat(300);
      const proposal = { ...card, ask: `Raise the cap.\n\n${note}`, action };

      it("shows the agent's note apart from the ask, as plain unverified text", async () => {
        renderCard(proposal);
        const box = await screen.findByTestId("decision-request-agent-note");
        expect(box).toHaveAccessibleName("Agent's note (not verified)");
        expect(box).toHaveTextContent("Need more runs");
        expect(box).toHaveTextContent("[click](https://evil.test)");
        expect(box.querySelector("a")).toBeNull();
        expect(box.querySelector("p")).toHaveClass("break-words", "whitespace-pre-wrap");
        const ask = screen.getByTestId("decision-request-ask");
        expect(ask).toHaveTextContent("Raise the cap.");
        expect(ask).not.toHaveTextContent("Need more runs");
        expect(ask).not.toHaveTextContent("not verified");
      });

      it("shows no note box for an ask without a note", () => {
        renderCard({ ...proposal, ask: "Raise the cap." });
        expect(screen.queryByTestId("decision-request-agent-note")).toBeNull();
      });

      it("keeps the outcome on the same card after the request turns answered", async () => {
        getDecisionActionOutcome.mockResolvedValue({ id: "action", state: "executed", result: { outcome: "cap_applied", from: 10, to: 30 } });
        renderCard({ ...proposal, state: "answered", action: { ...action, state: "executed", preview: undefined } });
        expect(await screen.findByText("Cap applied")).toBeInTheDocument();
        expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
      });

      it("goes from Approve to the outcome when the server announces the change", async () => {
        // The real handler for decision_answer:updated invalidates the DM message
        // query; the refetch carries the action's new state into the card.
        const qc = new QueryClient();
        let served: RequestCard = proposal;
        const Dm = () => {
          const { data } = useQuery({ queryKey: chatKeys.messages("s1"), queryFn: async () => served });
          return data ? <DecisionRequestCard card={data} /> : null;
        };
        renderWithI18n(<QueryClientProvider client={qc}><WorkspaceSlugProvider slug="acme"><Dm /></WorkspaceSlugProvider></QueryClientProvider>);
        await screen.findByRole("button", { name: "Approve with passkey" });
        expect(screen.getByRole("button", { name: "Reject" })).toBeInTheDocument();
        served = { ...proposal, state: "answered", action: { ...action, state: "failed", preview: undefined } };
        getDecisionActionOutcome.mockResolvedValue({ id: "action", state: "failed", error_code: "stale_base" });
        act(() => applyDecisionAnswerUpdated(qc, { answer_id: "a", issue_id: proposal.issue_id, state: "effective" }));
        expect(await screen.findByText("The target changed after the request. Ask again.")).toBeInTheDocument();
        expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
        expect(screen.queryByRole("button", { name: "Reject" })).toBeNull();
      });
    });

    it("shows nothing when the card has no action", () => {
      renderCard();
      expect(screen.queryByText("Details unavailable")).toBeNull();
      expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
    });
  });
});

describe("DecisionRequestCard passkey-gated answering", () => {
  const withOptions: RequestCard = { ...card, options: [{ label: "Option A" }, { label: "Option B" }] };
  const gated = () => new ApiError("assertion_required: sign this answer with your passkey", 403, "Forbidden");
  const challenge = { challenge_id: "ch-1", options: { publicKey: {} }, expires_at: "later", answer_revision: 3, policy_revision: 2 };
  const renderCard = () =>
    renderWithI18n(<QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={withOptions} /></WorkspaceSlugProvider></QueryClientProvider>);
  const pickAndSend = async (user: ReturnType<typeof userEvent.setup>) => {
    await user.type(screen.getByRole("textbox", { name: "Add a note (optional)" }), "why");
    editor.live = "why";
    await user.click(screen.getByRole("radio", { name: /Option A/ }));
    await user.click(screen.getByRole("button", { name: "Send" }));
  };
  beforeEach(() => {
    useDecisionAnswerDraftStore.setState({ drafts: {} });
    answerDecisionRequest.mockReset().mockRejectedValueOnce(gated()).mockResolvedValue(undefined);
    challengeDecisionAnswer.mockReset().mockResolvedValue(challenge);
    signPasskeyChallenge.mockReset().mockResolvedValue({ challenge_id: "ch-1", credential: { id: "cred" } });
    ui.desktop = false;
    openExternal.mockReset();
    editor.live = undefined;
  });

  it("asks for a passkey after assertion_required, then sends the signed answer with the revisions", async () => {
    renderCard();
    const user = userEvent.setup();
    await pickAndSend(user);
    expect(await screen.findByRole("alert")).toHaveTextContent("This answer needs your passkey");
    expect(challengeDecisionAnswer).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Send with passkey" }));
    await vi.waitFor(() => expect(answerDecisionRequest).toHaveBeenCalledTimes(2));
    expect(challengeDecisionAnswer).toHaveBeenCalledWith("request", { option: "Option A", text: "why" });
    expect(answerDecisionRequest).toHaveBeenLastCalledWith("request", {
      option: "Option A", text: "why", answer_revision: 3, policy_revision: 2,
      assertion: { challenge_id: "ch-1", credential: { id: "cred" } },
    });
  });

  it("keeps the clicked option when the note is edited before the passkey retry", async () => {
    renderCard();
    const user = userEvent.setup();
    await pickAndSend(user);
    await screen.findByRole("button", { name: "Send with passkey" });
    await user.type(screen.getByRole("textbox", { name: "Add a note (optional)" }), " more");
    editor.live = "why more";
    await user.click(screen.getByRole("button", { name: "Send with passkey" }));
    await vi.waitFor(() => expect(answerDecisionRequest).toHaveBeenCalledTimes(2));
    expect(challengeDecisionAnswer).toHaveBeenCalledWith("request", { option: "Option A", text: "why more" });
    expect(answerDecisionRequest).toHaveBeenLastCalledWith("request", expect.objectContaining({ option: "Option A", text: "why more" }));
  });

  it("keeps the draft and offers a retry when the passkey ceremony is cancelled", async () => {
    signPasskeyChallenge.mockRejectedValueOnce(new Error("passkey sign-in was cancelled"));
    renderCard();
    const user = userEvent.setup();
    await pickAndSend(user);
    await user.click(await screen.findByRole("button", { name: "Send with passkey" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("The passkey step did not finish");
    expect(answerDecisionRequest).toHaveBeenCalledTimes(1);
    expect(screen.queryByTestId("decision-request-kept-draft")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Send with passkey" }));
    await vi.waitFor(() => expect(answerDecisionRequest).toHaveBeenCalledTimes(2));
  });

  it("shows a reload message on a stale revision and signs a fresh challenge on retry", async () => {
    answerDecisionRequest.mockReset()
      .mockRejectedValueOnce(gated())
      .mockRejectedValueOnce(new ApiError("stale_answer: the request or workspace policy changed; reload the card", 409, "Conflict"))
      .mockResolvedValue(undefined);
    renderCard();
    const user = userEvent.setup();
    await pickAndSend(user);
    await user.click(await screen.findByRole("button", { name: "Send with passkey" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("changed since you opened it");
    expect(screen.queryByTestId("decision-request-kept-draft")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Send with passkey" }));
    await vi.waitFor(() => expect(answerDecisionRequest).toHaveBeenCalledTimes(3));
    expect(challengeDecisionAnswer).toHaveBeenCalledTimes(2);
  });

  it("hands off to the web page on desktop instead of running the ceremony", async () => {
    ui.desktop = true;
    renderCard();
    const user = userEvent.setup();
    await pickAndSend(user);
    await user.click(await screen.findByRole("button", { name: "Open in browser" }));
    expect(openExternal).toHaveBeenCalledWith("https://app.test/acme/issues/issue");
    expect(challengeDecisionAnswer).not.toHaveBeenCalled();
    expect(signPasskeyChallenge).not.toHaveBeenCalled();
  });
});
