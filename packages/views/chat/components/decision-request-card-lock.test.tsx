import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { forwardRef } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { configureShortcutPlatform } from "@multica/core/shortcuts";
import { useDecisionAnswerDraftStore } from "@multica/core/issues/stores/decision-answer-draft-store";
import { renderWithI18n } from "../../test/i18n";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
import { DecisionRequestCard } from "./decision-request-card";

// The open card against the B v9 lock (design/decision-card/approved/direction-b.html, states s2, s4, s6; CHE-9096).
// check-fidelity reads these blocks by data-testid; this file pins the same structure at unit level.

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
vi.mock("@multica/core/auth/webauthn", () => ({ signPasskeyChallenge: vi.fn() }));
const api = vi.hoisted(() => ({
  answerDecisionRequest: vi.fn(),
  listDecisionArtifacts: vi.fn(),
  getDecisionActionOutcome: vi.fn(),
  listRecentRuns: vi.fn(),
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
type EditorProps = { defaultValue?: string; ariaLabel?: string; onUpdate?: (markdown: string) => void };
vi.mock("./lazy-content-editor", () => ({
  LazyContentEditor: forwardRef(function MockEditor(props: EditorProps, ref) {
    let box: HTMLTextAreaElement | null = null;
    const handle = { hasActiveUploads: () => false, getMarkdown: () => box?.value ?? props.defaultValue ?? "", focus: () => box?.focus() };
    if (typeof ref === "function") ref(handle as never);
    else if (ref) (ref as { current: unknown }).current = handle;
    return <textarea ref={(node) => { box = node; }} aria-label={props.ariaLabel} defaultValue={props.defaultValue ?? ""} onChange={(event) => props.onUpdate?.(event.target.value)} />;
  }),
}));

const budget: RequestCard = {
  id: "request", issue_id: "issue", issue_identifier: "CHE-3461", issue_title: "Sync v0.8.0", asking_agent_name: "x99-claude-gstack",
  ask: "Reopen CHE-3461 so the v0.8.0 sync can finish?\n\nIf you do nothing, CHE-3461 stays paused and nothing else changes.",
  state: "open", created_at: "2026-11-11T06:20:00Z",
  action: {
    id: "action", action_type: "issue.run_budget.top_up", payload_sha256: "p", state: "pending",
    preview: { sha256: "a".repeat(64), fields: [{ label: "Runs used", current: "44 of 25", proposed: "0 of 25" }] },
  },
};

const view = (card: RequestCard) => renderWithI18n(
  <QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={card} /></WorkspaceSlugProvider></QueryClientProvider>,
);
const text = (id: string) => screen.getByTestId(id).textContent?.replace(/\s+/g, " ").trim();

describe("open decision card against the lock (s2, s4, s6)", () => {
  beforeEach(() => {
    configureShortcutPlatform("macos");
    sessionStorage.clear();
    useDecisionAnswerDraftStore.setState({ drafts: {} });
    api.listDecisionArtifacts.mockResolvedValue([]);
    api.getDecisionActionOutcome.mockReset().mockResolvedValue({ id: "action", state: "pending" });
    api.listRecentRuns.mockReset().mockResolvedValue([]);
  });

  it("draws the title inside the asker row: title, agent, state tag, issue chip", async () => {
    view(budget);
    await screen.findByTestId("decision-request-choice");
    expect(text("decision-request-title")).toBe("Decision request");
    expect(text("decision-request-asker")).toBe("Decision request from x99-claude-gstack OpenCHE-3461");
    expect(within(screen.getByTestId("decision-request-asker")).getByTestId("decision-request-title")).toBeTruthy();
  });

  it("keeps only the question in the ask block and the rest in the effect block", async () => {
    view(budget);
    await screen.findByTestId("decision-request-choice");
    expect(text("decision-request-ask")).toBe("Reopen CHE-3461 so the v0.8.0 sync can finish?");
    expect(text("decision-request-effect")).toBe("If you do nothing, CHE-3461 stays paused and nothing else changes.");
  });

  it("draws the choice as one block with the lock's keycap on the one button", async () => {
    view(budget);
    const choice = await screen.findByTestId("decision-request-choice");
    expect(choice.textContent).toBe("ApproveReject");
    expect(screen.getByRole("button", { name: "Discuss more (⌘ + ⏎)" })).toBeTruthy();
  });

  it("states what Discuss more does: with nothing typed, and with a message typed", async () => {
    view(budget);
    const card = await screen.findByTestId("decision-request-card");
    expect(card.textContent).toContain("With nothing typed, this puts the cursor in the message box.");
    await userEvent.setup().type(screen.getByRole("textbox", { name: "Message" }), "Is CI on main green yet?");
    await waitFor(() => expect(card.textContent).toContain("Does not approve or reject."));
    expect(card.textContent).not.toContain("With nothing typed");
  });

  it("leaves the summary and the preview out while a message is typed (s2)", async () => {
    view(budget);
    await userEvent.setup().type(await screen.findByRole("textbox", { name: "Message" }), "Is CI on main green yet?");
    await waitFor(() => expect(screen.queryByTestId("decision-request-effect")).toBeNull());
    expect(screen.queryByTestId("decision-request-action-preview")).toBeNull();
  });

  it("draws Approve chosen with a note as the lock's s4: no summary, no preview, passkey keycap", async () => {
    const user = userEvent.setup();
    view(budget);
    await user.click(await screen.findByRole("radio", { name: "Approve" }));
    await user.type(await screen.findByRole("textbox", { name: "Note (optional)" }), "ok, go ahead");
    await screen.findByRole("button", { name: "Approve with passkey (⌘ + ⏎)" });
    expect(screen.queryByTestId("decision-request-effect")).toBeNull();
    expect(screen.queryByTestId("decision-request-action-preview")).toBeNull();
  });

  it("draws Reject chosen with a reason as the lock's s6", async () => {
    const user = userEvent.setup();
    view(budget);
    await user.click(await screen.findByRole("radio", { name: "Reject" }));
    await user.type(await screen.findByRole("textbox", { name: "Reason (optional)" }), "Not until CI on main is green.");
    await screen.findByRole("button", { name: "Confirm Reject (⌘ + ⏎)" });
    expect(screen.queryByTestId("decision-request-effect")).toBeNull();
    expect(screen.queryByTestId("decision-request-action-preview")).toBeNull();
  });

  it("shows no timestamp on an open card", async () => {
    const { container } = view(budget);
    await screen.findByTestId("decision-request-choice");
    expect(container.querySelector("time")).toBeNull();
  });

  it("keeps the preview visible while nothing is typed", async () => {
    view(budget);
    await screen.findByTestId("decision-request-choice");
    expect(screen.getByTestId("decision-request-action-preview")).toBeTruthy();
  });
});
