import { screen } from "@testing-library/react";
import { forwardRef } from "react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@multica/core/api";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderWithI18n } from "../../test/i18n";
import { useDecisionAnswerDraftStore } from "@multica/core/issues/stores/decision-answer-draft-store";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
import { DecisionRequestCard } from "./decision-request-card";
import { IssueDecisionBanner } from "../../issues/components/issue-decision-banner";

// CHE-3521: the answer draft (option + note) lives in the keyed draft store, so
// an unmount (virtualized scroll-out, route change, reload) cannot drop it.

vi.mock("@multica/core/hooks", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/hooks")>()),
  useWorkspaceId: () => "ws",
}));
vi.mock("../../navigation", async (importActual) => ({
  ...(await importActual<typeof import("../../navigation")>()),
  AppLink: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => <a href={href} {...rest}>{children}</a>,
}));
vi.mock("../../issues/components/issue-chip", () => ({
  IssueChip: ({ fallbackLabel }: { fallbackLabel?: string }) => <span>{fallbackLabel}</span>,
}));

const api = vi.hoisted(() => ({
  answerDecisionRequest: vi.fn(),
  listDecisionArtifacts: vi.fn(),
  listOpenDecisionRequests: vi.fn(),
  listTasksByIssue: vi.fn(),
}));
vi.mock("@multica/core/api", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/api")>()),
  api,
}));

type EditorProps = { defaultValue?: string; ariaLabel?: string; onUpdate?: (markdown: string) => void; onSubmit?: () => void };
vi.mock("./lazy-content-editor", () => ({
  LazyContentEditor: forwardRef(function MockEditor(props: EditorProps, ref) {
    const handle = { hasActiveUploads: () => false, getMarkdown: () => props.defaultValue ?? "" };
    if (typeof ref === "function") ref(handle as never);
    else if (ref) (ref as { current: unknown }).current = handle;
    return <textarea aria-label={props.ariaLabel} defaultValue={props.defaultValue ?? ""} onChange={(event) => props.onUpdate?.(event.target.value)} />;
  }),
}));

const options = [{ label: "Option A", what_happens: "A happens" }, { label: "Option B", what_happens: "B happens" }];
const card = (id: string): RequestCard => ({
  id, issue_id: "issue", issue_identifier: "CHE-1", source_comment_id: "source",
  ask: "Pick one.", state: "open", created_at: "2026-10-26T23:30:00Z", options,
});
const note = () => screen.getByRole("textbox", { name: "Add a note (optional)" });

const chat = (value: RequestCard) => (
  <QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={value} /></WorkspaceSlugProvider></QueryClientProvider>
);
const banner = () => (
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <WorkspaceSlugProvider slug="acme"><IssueDecisionBanner issueId="issue" status="need_decision" ownerAgentId="owner" /></WorkspaceSlugProvider>
  </QueryClientProvider>
);

describe("decision card draft survives remount", () => {
  beforeEach(() => {
    vi.resetAllMocks();
    useDecisionAnswerDraftStore.setState({ drafts: {} });
    api.listDecisionArtifacts.mockResolvedValue([]);
    api.listTasksByIssue.mockResolvedValue([]);
    api.answerDecisionRequest.mockResolvedValue({});
  });

  it("chat surface: option and note come back after unmount and remount", async () => {
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card("chat-remount")));
    await user.click(screen.getByRole("radio", { name: /Option B/ }));
    await user.type(note(), "my note");
    first.unmount();

    renderWithI18n(chat(card("chat-remount")));
    expect(screen.getByRole("radio", { name: /Option B/ })).toBeChecked();
    expect(note()).toHaveValue("my note");
  });

  it("issue banner surface: option and note come back after unmount and remount", async () => {
    api.listOpenDecisionRequests.mockResolvedValue([{ ...card("banner-remount"), asking_agent_id: "a", asking_agent_name: "Terra", escalation_class: "spend", chat_session_id: "s", message_id: null }]);
    const user = userEvent.setup();
    const first = renderWithI18n(banner());
    await user.click(await screen.findByRole("radio", { name: /Option A/ }));
    await user.type(note(), "banner note");
    first.unmount();

    renderWithI18n(banner());
    expect(await screen.findByRole("radio", { name: /Option A/ })).toBeChecked();
    expect(note()).toHaveValue("banner note");
  });

  it("a draft typed in the chat card shows in the banner card for the same request", async () => {
    api.listOpenDecisionRequests.mockResolvedValue([{ ...card("shared"), asking_agent_id: "a", asking_agent_name: "Terra", escalation_class: "spend", chat_session_id: "s", message_id: null }]);
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card("shared")));
    await user.type(note(), "typed in chat");
    first.unmount();

    renderWithI18n(banner());
    await screen.findByRole("radio", { name: /Option A/ });
    expect(note()).toHaveValue("typed in chat");
  });

  it("a refused send keeps the draft across remount", async () => {
    api.answerDecisionRequest.mockRejectedValue(new ApiError("refused", 500, "Error"));
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card("refused")));
    await user.click(screen.getByRole("radio", { name: /Option A/ }));
    await user.click(screen.getByRole("button", { name: "Send" }));
    await vi.waitFor(() => expect(api.answerDecisionRequest).toHaveBeenCalledTimes(1));
    first.unmount();

    renderWithI18n(chat(card("refused")));
    expect(screen.getByRole("radio", { name: /Option A/ })).toBeChecked();
  });

  it("a successful send clears the draft, even when the card unmounted mid-send", async () => {
    let resolve: (value?: unknown) => void = () => { throw new Error("not started"); };
    api.answerDecisionRequest.mockReturnValue(new Promise((r) => { resolve = r; }));
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card("sent")));
    await user.click(screen.getByRole("radio", { name: /Option B/ }));
    await user.click(screen.getByRole("button", { name: "Send" }));
    await vi.waitFor(() => expect(api.answerDecisionRequest).toHaveBeenCalledTimes(1));
    first.unmount();
    resolve();
    await vi.waitFor(() => expect(useDecisionAnswerDraftStore.getState().hasDraft("decision:sent")).toBe(false));
  });

  it("Discard draft clears it for good", async () => {
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card("discard")));
    await user.click(screen.getByRole("radio", { name: /Option A/ }));
    first.rerender(chat({ ...card("discard"), state: "answered" }));
    await user.click(screen.getByRole("button", { name: "Discard draft" }));
    first.unmount();

    renderWithI18n(chat(card("discard")));
    expect(screen.getByRole("radio", { name: /Option A/ })).not.toBeChecked();
  });
});
