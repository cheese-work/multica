import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import type { ChatScheduledWakeup } from "@multica/core/types";
import { NavigationProvider } from "../../navigation";
import { RESOURCES } from "../../locales";

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: () => "x99-claude-opus" }),
}));
vi.mock("../../issues/components/issue-chip", () => ({
  IssueChip: ({ issueId, fallbackLabel }: { issueId: string; fallbackLabel?: string }) => (
    <span data-testid="issue-chip" data-issue-id={issueId}>{fallbackLabel}</span>
  ),
}));
vi.mock("../../navigation", async (importActual) => ({
  ...(await importActual<typeof import("../../navigation")>()),
  AppLink: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => <a href={href} {...rest}>{children}</a>,
}));

import { WakeupEventRow } from "./wakeup-event-row";

const HEADER = "[Scheduled wakeup";
// 10:00Z is 17:00 in Ho Chi Minh (ICT); created 03:10Z is 10:10 ICT.
const wakeup = (extra: Partial<ChatScheduledWakeup> = {}): ChatScheduledWakeup => ({
  id: "w1",
  agent_id: "agent-1",
  title: "Report 17:00: decision A for CHE-2321",
  instruction: "17:00 ICT report for Cheese, decision A on CHE-2321. Also check CHE-2233 and CHE-2321 again.\nSecond line.",
  fire_at: "2026-10-29T10:00:00Z",
  created_at: "2026-10-29T03:10:00Z",
  ...extra,
});

function renderRow(w: ChatScheduledWakeup = wakeup()) {
  return render(
    <I18nProvider locale="en" resources={RESOURCES}>
      <WorkspaceSlugProvider slug="acme">
        <NavigationProvider value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/chat", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (p) => p }}>
          <WakeupEventRow wakeup={w} timeZone="Asia/Ho_Chi_Minh" />
        </NavigationProvider>
      </WorkspaceSlugProvider>
    </I18nProvider>,
  );
}

describe("WakeupEventRow", () => {
  it("renders the collapsed thread-rail row", () => {
    renderRow();
    expect(screen.getByRole("img", { name: "Scheduled wakeup fired" })).toBeInTheDocument();
    expect(screen.getByText("Scheduled 17:00 ICT")).toBeInTheDocument();
    expect(screen.getByText("x99-claude-opus set this at 10:10 ICT")).toBeInTheDocument();
    expect(screen.getByText("Report 17:00: decision A for CHE-2321")).toBeInTheDocument();
    const toggle = screen.getByRole("button", { name: "Show instruction" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveClass("max-md:min-h-11");
    expect(screen.queryByRole("region")).not.toBeInTheDocument();
    expect(screen.queryByText(HEADER, { exact: false })).not.toBeInTheDocument();
  });

  it("expands to the verbatim instruction in a named region wired to the toggle", () => {
    renderRow();
    const toggle = screen.getByRole("button", { name: "Show instruction" });
    fireEvent.click(toggle);
    const region = screen.getByRole("region", { name: "Instruction for the agent" });
    expect(toggle).toHaveAccessibleName("Hide instruction");
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(toggle).toHaveAttribute("aria-controls", region.id);
    expect(region.textContent).toContain("17:00 ICT report for Cheese, decision A on CHE-2321");
    expect(region.textContent).toContain("Second line.");
    expect(screen.queryByText(HEADER, { exact: false })).not.toBeInTheDocument();
    fireEvent.click(toggle);
    expect(screen.queryByRole("region")).not.toBeInTheDocument();
  });

  it("falls back to the first instruction line, as a single muted line, when there is no title", () => {
    renderRow(wakeup({ title: "" }));
    const title = screen.getByTestId("wakeup-title");
    expect(title).toHaveTextContent("17:00 ICT report for Cheese, decision A on CHE-2321. Also check CHE-2233 and CHE-2321 again.");
    expect(title).not.toHaveTextContent("Second line.");
    expect(title).toHaveClass("truncate", "text-muted-foreground", "font-medium");
  });

  it("shows each distinct issue once as a chip link", () => {
    renderRow();
    fireEvent.click(screen.getByRole("button", { name: "Show instruction" }));
    const region = screen.getByRole("region");
    const links = within(region).getAllByRole("link");
    expect(links.map((l) => [l.textContent, l.getAttribute("href")])).toEqual([
      ["CHE-2321", "/acme/issues/CHE-2321"],
      ["CHE-2233", "/acme/issues/CHE-2233"],
    ]);
    // The repeat stays as plain text.
    expect(region.textContent).toContain("and CHE-2321 again");
  });

  it("renders mention links through the shared issue chip, not as raw markdown", () => {
    const id = "0199c0de-0000-4000-8000-000000000001";
    renderRow(wakeup({ title: "T", instruction: `Check [CHE-9](mention://issue/${id}) now.` }));
    fireEvent.click(screen.getByRole("button", { name: "Show instruction" }));
    const chip = screen.getByTestId("issue-chip");
    expect(chip).toHaveAttribute("data-issue-id", id);
    expect(chip).toHaveTextContent("CHE-9");
    expect(screen.getByRole("region").textContent).not.toContain("mention://");
  });

  it("keeps a zero-title, single-line instruction usable", () => {
    renderRow(wakeup({ title: "", instruction: "ping" }));
    expect(screen.getByTestId("wakeup-title")).toHaveTextContent("ping");
  });
});
