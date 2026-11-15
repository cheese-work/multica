import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { forwardRef } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useDecisionAnswerDraftStore } from "@multica/core/issues/stores/decision-answer-draft-store";
import { renderWithI18n } from "../../test/i18n";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
import { DecisionRequestCard } from "./decision-request-card";

// Direction B v9 (design/decision-card/approved/direction-b.html): Approve / Reject choice, one composer
// with three modes, one button that names the result, JSON behind Details, decided cards as one block.

vi.mock("../../platform", () => ({ openExternal: vi.fn() }));
vi.mock("../../platform/local-directory", () => ({ isDesktopShell: () => false }));
vi.mock("../../navigation", async (importActual) => ({
  ...(await importActual<typeof import("../../navigation")>()),
  useOptionalNavigation: () => null,
  AppLink: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => <a href={href} {...rest}>{children}</a>,
}));
vi.mock("../../issues/components/issue-chip", () => ({
  IssueChip: ({ fallbackLabel, children }: { fallbackLabel?: string; children?: React.ReactNode }) => <span>{children ?? fallbackLabel}</span>,
}));
vi.mock("../../issues/components/comment-mention-card", () => ({
  CommentMentionCard: ({ fallback }: { fallback: React.ReactNode }) => <>{fallback}</>,
}));
const signPasskeyChallenge = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/auth/webauthn", () => ({ signPasskeyChallenge }));
const api = vi.hoisted(() => ({
  answerDecisionRequest: vi.fn(),
  listDecisionArtifacts: vi.fn(),
  getDecisionActionOutcome: vi.fn(),
  challengeDecisionAction: vi.fn(),
  approveDecisionAction: vi.fn(),
  rejectDecisionAction: vi.fn(),
}));
vi.mock("@multica/core/api", async (importActual) => ({ ...(await importActual<typeof import("@multica/core/api")>()), api }));
vi.mock("@multica/core/permissions", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/permissions")>()),
  useCurrentMember: () => ({ userId: "u1", role: "owner", member: null, isLoading: false }),
}));
vi.mock("@multica/core/paths", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/paths")>()),
  useCurrentWorkspace: () => ({ id: "ws-1" }),
}));

// Stand-in for Tiptap: Mod+Enter submits, plain Enter only adds a line (the shortcut system's default).
type EditorProps = { defaultValue?: string; ariaLabel?: string; onUpdate?: (markdown: string) => void; onSubmit?: () => void };
vi.mock("./lazy-content-editor", () => ({
  LazyContentEditor: forwardRef(function MockEditor(props: EditorProps, ref) {
    let box: HTMLTextAreaElement | null = null;
    const handle = {
      hasActiveUploads: () => false,
      getMarkdown: () => box?.value ?? props.defaultValue ?? "",
      focus: () => box?.focus(),
    };
    if (typeof ref === "function") ref(handle as never);
    else if (ref) (ref as { current: unknown }).current = handle;
    return (
      <textarea
        ref={(node) => { box = node; }}
        aria-label={props.ariaLabel}
        defaultValue={props.defaultValue ?? ""}
        onChange={(event) => props.onUpdate?.(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && (event.ctrlKey || event.metaKey)) {
            event.preventDefault();
            props.onSubmit?.();
          }
        }}
      />
    );
  }),
}));

const change = {
  field: "workspace.context", target_id: "ws-1", keys: [] as string[], diff_kind: "text" as const,
  diff: [{ old_start: 1, new_start: 1, lines: ["-old rule", "+new rule"] }],
  after: "new rule", before_sha256: "b".repeat(64), after_sha256: "c".repeat(64), sha256: "e".repeat(64),
};
const action = { id: "action", action_type: "workspace.context.set", payload_sha256: "payload", config_change: change, owner_only: true };
const open: RequestCard = {
  id: "request", issue_id: "issue", issue_identifier: "CHE-3461", issue_title: "Sync", asking_agent_name: "x99-claude-gstack",
  ask: "Reopen CHE-3461 so the v0.8.0 sync can finish?\n\nIf you do nothing, CHE-3461 stays paused.",
  state: "open", created_at: "2026-11-11T06:20:00Z", action,
};
const decided = (state: string, extra: Partial<RequestCard> = {}): RequestCard => ({
  ...open, state: "answered", record_comment_id: "record", action: { ...action, state: state as never, config_change: undefined },
  answer: { state: "picked_up", answered_at: "2026-11-11T06:41:00Z", run: { id: "t", agent_name: "Sol" } },
  ...extra,
});

const view = (card: RequestCard) => renderWithI18n(
  <QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={card} /></WorkspaceSlugProvider></QueryClientProvider>,
);
const choice = (name: string) => screen.findByRole("radio", { name });
const primary = (name: RegExp) => screen.findByRole("button", { name });

describe("DecisionRequestCard direction B v9", () => {
  beforeEach(() => {
    sessionStorage.clear();
    useDecisionAnswerDraftStore.setState({ drafts: {} });
    api.listDecisionArtifacts.mockResolvedValue([]);
    api.getDecisionActionOutcome.mockReset().mockResolvedValue({ id: "action", state: "pending" });
    api.answerDecisionRequest.mockReset().mockResolvedValue(undefined);
    api.challengeDecisionAction.mockReset().mockResolvedValue({ challenge_id: "ch" });
    api.approveDecisionAction.mockReset().mockResolvedValue({ id: "action", state: "executed" });
    api.rejectDecisionAction.mockReset().mockResolvedValue({ id: "action", state: "rejected" });
    signPasskeyChallenge.mockReset().mockResolvedValue({ id: "cred" });
  });

  it("puts the plain-language ask first and keeps the JSON behind a Details control", async () => {
    view(open);
    const details = await screen.findByRole("button", { name: /Details/ });
    expect(details).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByTestId("config-change-viewer")).toBeNull();
    const ask = screen.getByTestId("decision-request-ask");
    expect(ask.compareDocumentPosition(details) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    await userEvent.setup().click(details);
    expect(details).toHaveAttribute("aria-expanded", "true");
    expect(await screen.findByTestId("config-change-viewer")).toBeInTheDocument();
    expect(screen.getByText(/workspace\.context\.set/)).toBeInTheDocument();
  });

  it("offers Approve and Reject as one radiogroup with nothing chosen, arrow keys, and Space to clear", async () => {
    const user = userEvent.setup();
    view(open);
    const group = await screen.findByRole("radiogroup", { name: "Your decision" });
    const approve = await choice("Approve");
    const reject = await choice("Reject");
    expect(group).toContainElement(approve);
    expect(approve).toHaveAttribute("aria-checked", "false");
    expect(reject).toHaveAttribute("aria-checked", "false");
    expect(approve).toHaveAttribute("tabindex", "0");
    expect(reject).toHaveAttribute("tabindex", "-1");
    approve.focus();
    await user.keyboard("{ArrowRight}");
    expect(reject).toHaveFocus();
    expect(reject).toHaveAttribute("aria-checked", "true");
    expect(reject).toHaveAttribute("tabindex", "0");
    expect(approve).toHaveAttribute("tabindex", "-1");
    await user.keyboard("{ArrowLeft}");
    expect(approve).toHaveAttribute("aria-checked", "true");
    await user.keyboard(" ");
    expect(approve).toHaveAttribute("aria-checked", "false");
    await user.click(reject);
    await user.click(reject);
    expect(reject).toHaveAttribute("aria-checked", "false");
  });

  it("names the composer and the one button after the choice, with a passkey icon on Approve", async () => {
    const user = userEvent.setup();
    view(open);
    expect(await screen.findByRole("textbox", { name: "Message" })).toBeInTheDocument();
    expect(await primary(/^Discuss more \(.+\)$/)).toBeEnabled();
    await user.click(await choice("Approve"));
    expect(screen.getByRole("textbox", { name: "Note (optional)" })).toBeInTheDocument();
    const approveButton = await primary(/^Approve with passkey \(.+\)$/);
    expect(approveButton.querySelector("svg.icon-key-round")).not.toBeNull();
    await user.click(await choice("Reject"));
    expect(screen.getByRole("textbox", { name: "Reason (optional)" })).toBeInTheDocument();
    const rejectButton = await primary(/^Confirm Reject \(.+\)$/);
    expect(rejectButton.querySelector("svg.icon-key-round")).toBeNull();
  });

  it("presses the button for the choice on Mod+Enter and never on plain Enter", async () => {
    const user = userEvent.setup();
    view(open);
    await user.click(await choice("Approve"));
    await primary(/^Approve with passkey/);
    const box = screen.getByRole("textbox", { name: "Note (optional)" });
    await user.type(box, "{Enter}");
    expect(api.challengeDecisionAction).not.toHaveBeenCalled();
    await user.type(box, "{Control>}{Enter}{/Control}");
    await waitFor(() => expect(api.approveDecisionAction).toHaveBeenCalledOnce());
    expect(api.approveDecisionAction).toHaveBeenCalledWith("action", expect.objectContaining({ payload_sha256: "payload" }));
  });

  it("rejects from Confirm Reject without a passkey", async () => {
    const user = userEvent.setup();
    view(open);
    await user.click(await choice("Reject"));
    await user.click(await primary(/^Confirm Reject/));
    await waitFor(() => expect(api.rejectDecisionAction).toHaveBeenCalledWith("action"));
    expect(signPasskeyChallenge).not.toHaveBeenCalled();
  });

  it("Discuss more on an empty box focuses the composer and sends nothing; with text it sends the message", async () => {
    const user = userEvent.setup();
    view(open);
    await user.click(await primary(/^Discuss more/));
    expect(screen.getByRole("textbox", { name: "Message" })).toHaveFocus();
    expect(api.answerDecisionRequest).not.toHaveBeenCalled();
    await user.type(screen.getByRole("textbox", { name: "Message" }), "Is CI green yet?");
    await user.click(screen.getByRole("button", { name: /^Discuss more/ }));
    await waitFor(() => expect(api.answerDecisionRequest).toHaveBeenCalledOnce());
    expect(api.answerDecisionRequest).toHaveBeenCalledWith("request", expect.objectContaining({ text: "Is CI green yet?" }));
    expect(api.answerDecisionRequest.mock.calls[0]![1].option).toBeUndefined();
  });

  it("keeps the unsent composer text across an unmount until it is sent or discarded", async () => {
    const user = userEvent.setup();
    const { unmount } = view(open);
    await user.type(await screen.findByRole("textbox", { name: "Message" }), "half a thought");
    unmount();
    view(open);
    expect(await screen.findByRole("textbox", { name: "Message" })).toHaveValue("half a thought");
  });

  describe("decided cards collapse to one block with the state as icon and word", () => {
    it.each([
      ["approved and applied", decided("executed"), "approved", "Approved", "Applied."],
      ["approved but not applied", decided("failed"), "approved", "Approved", "Not applied."],
      ["rejected", decided("rejected"), "rejected", "Rejected", "Nothing ran."],
    ])("%s", async (_name, card, state, word, effect) => {
      api.getDecisionActionOutcome.mockResolvedValue({ id: "action", state: card.action!.state });
      view(card);
      expect(screen.getByTestId("decision-request-card")).toHaveAttribute("data-state", state);
      const toggle = screen.getByTestId("decision-request-fold-toggle");
      expect(toggle).toHaveAttribute("aria-expanded", "false");
      expect(toggle).toHaveTextContent(word);
      expect(toggle).toHaveTextContent("Reopen CHE-3461 so the v0.8.0 sync can finish?");
      expect(toggle.querySelector("svg.icon")).not.toBeNull();
      expect(screen.getByTestId("decision-request-effect")).toHaveTextContent(effect);
      expect(screen.getByTestId("decision-request-meta")).toHaveTextContent("11 Nov 2026, 13:20 ICT");
      expect(screen.getByTestId("decision-request-meta")).toHaveTextContent("x99-claude-gstack");
      expect(screen.queryByRole("radiogroup")).toBeNull();
      expect(screen.queryByTestId("decision-request-answer")).toBeNull();
    });

    it("renders the open state without a fold", async () => {
      view(open);
      expect(screen.getByTestId("decision-request-card")).toHaveAttribute("data-state", "open");
      expect(screen.queryByTestId("decision-request-fold-toggle")).toBeNull();
      expect(screen.queryByTestId("decision-request-effect")).toBeNull();
    });
  });
});
