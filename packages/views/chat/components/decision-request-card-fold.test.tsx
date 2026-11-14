import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { forwardRef } from "react";
import { beforeEach, afterEach, describe, expect, it, vi } from "vitest";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderWithI18n } from "../../test/i18n";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
import { DecisionRequestCard } from "./decision-request-card";

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
const listDecisionArtifacts = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/api", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/api")>()),
  api: { listDecisionArtifacts },
}));
vi.mock("./lazy-content-editor", () => ({
  LazyContentEditor: forwardRef(function MockEditor() {
    return <textarea aria-label="note" />;
  }),
}));

const base: RequestCard = {
  id: "request", issue_id: "issue", issue_identifier: "CHE-1", issue_title: "Gantt arrows",
  ask: "Which arrow style?\n\nTwo prototypes are on the canvas.",
  state: "open", created_at: "2026-11-07T07:02:00Z",
  options: [{ label: "Arrows over bars" }],
};
const answered = (state: "picked_up" | "delivered" | "disclaimed", extra: Partial<RequestCard> = {}): RequestCard => ({
  ...base, state: "answered", record_comment_id: "record",
  answer: { state, chosen_option_index: 0, option_label: "Arrows over bars", answered_at: "2026-11-07T07:02:00Z", ...(state === "picked_up" ? { run: { id: "t", agent_name: "Sol" } } : {}) },
  ...extra,
});
const folded: [string, RequestCard][] = [
  ["picked_up", answered("picked_up")],
  ["withdrawn", { ...base, state: "withdrawn", withdrawn_at: "2026-11-07T07:10:00Z", reason: "Superseded" }],
  ["disclaimed", answered("disclaimed")],
];

const view = (card: RequestCard) => renderWithI18n(
  <QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={card} /></WorkspaceSlugProvider></QueryClientProvider>,
);
const toggle = () => screen.queryByRole("button", { name: /Which arrow style\?/ });
const body = () => screen.getByTestId("decision-request-card").querySelector("[data-fold-body]")!;

describe("DecisionRequestCard fold", () => {
  beforeEach(() => listDecisionArtifacts.mockResolvedValue([]));
  afterEach(() => vi.useRealTimers());

  it.each(folded)("renders %s folded at load: one button row, body inert, no transition class in play", (_, card) => {
    vi.useFakeTimers({ now: new Date("2026-11-07T07:03:00Z"), toFake: ["Date", "setTimeout", "clearTimeout"] });
    view(card);
    const row = toggle()!;
    expect(row).toHaveAttribute("aria-expanded", "false");
    expect(row).toHaveTextContent("Which arrow style?");
    expect(row.querySelector("time")).toHaveTextContent(card.state === "withdrawn" ? "07 Nov 2026, 14:10 ICT" : "07 Nov 2026, 14:02 ICT");
    expect(body()).toHaveAttribute("inert");
    expect(body()).toHaveAttribute("data-folded", "true");
  });

  it.each([
    ["picked_up", answered("picked_up"), "icon-check"],
    ["withdrawn", { ...base, state: "withdrawn", withdrawn_at: "2026-11-07T07:10:00Z" } as RequestCard, "icon-undo2"],
    ["disclaimed", answered("disclaimed"), "icon-ban"],
  ])("marks a %s folded row with its own state icon", (_, card, icon) => {
    vi.useFakeTimers({ now: new Date("2026-11-07T07:03:00Z"), toFake: ["Date", "setTimeout", "clearTimeout"] });
    view(card);
    expect(toggle()!.querySelector("svg.icon")).toHaveClass(icon);
  });

  it("shows the state word, option chip and time in the folded row", () => {
    vi.useFakeTimers({ now: new Date("2026-11-07T07:03:00Z"), toFake: ["Date", "setTimeout", "clearTimeout"] });
    view(answered("picked_up"));
    const row = toggle()!;
    expect(row).toHaveTextContent("Picked up");
    expect(row).toHaveTextContent("Option 1");
    expect(row).toHaveTextContent("Arrows over bars");
    expect(row.querySelector("time")).toHaveAttribute("datetime", "2026-11-07T07:02:00Z");
  });

  it("expands on click and on Enter, then folds again; the state is not persisted", async () => {
    const user = userEvent.setup();
    const { unmount } = view(answered("picked_up"));
    await user.click(toggle()!);
    expect(toggle()).toHaveAttribute("aria-expanded", "true");
    expect(body()).not.toHaveAttribute("inert");
    await user.click(toggle()!);
    expect(toggle()).toHaveAttribute("aria-expanded", "false");
    toggle()!.focus();
    await user.keyboard("{Enter}");
    expect(toggle()).toHaveAttribute("aria-expanded", "true");
    unmount();
    view(answered("picked_up"));
    expect(toggle()).toHaveAttribute("aria-expanded", "false");
  });

  it("keeps an open card and a not_picked_up answer expanded, with no fold button", () => {
    vi.useFakeTimers({ now: new Date("2026-11-07T07:10:00Z"), toFake: ["Date", "setTimeout", "clearTimeout"] });
    const { unmount } = view(base);
    expect(toggle()).toBeNull();
    expect(body()).not.toHaveAttribute("inert");
    unmount();
    view(answered("delivered"));
    expect(screen.getByTestId("decision-request-card")).toHaveAttribute("data-state", "not_picked_up");
    expect(toggle()).toBeNull();
    expect(body()).not.toHaveAttribute("inert");
  });

  it("folds a card that turns picked_up while mounted, keeping the body mounted", () => {
    vi.useFakeTimers({ now: new Date("2026-11-07T07:03:00Z"), toFake: ["Date", "setTimeout", "clearTimeout"] });
    const { rerender } = view(answered("delivered"));
    expect(toggle()).toBeNull();
    const before = body();
    rerender(
      <QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={answered("picked_up")} /></WorkspaceSlugProvider></QueryClientProvider>,
    );
    expect(toggle()).toHaveAttribute("aria-expanded", "false");
    expect(body()).toBe(before);
    expect(body()).toHaveAttribute("inert");
  });

  it("animates height and opacity ease-out (240 ms collapse, 200 ms expand, set inline), off under reduced motion", () => {
    vi.useFakeTimers({ now: new Date("2026-11-07T07:03:00Z"), toFake: ["Date", "setTimeout", "clearTimeout"] });
    view(answered("picked_up"));
    const cls = body().className;
    expect((body() as HTMLElement).style.transitionDuration).toBe("240ms");
    expect(cls).toContain("ease-out");
    expect(cls).toContain("transition-[grid-template-rows,opacity]");
    expect(cls).toContain("motion-reduce:transition-none");
  });
});
