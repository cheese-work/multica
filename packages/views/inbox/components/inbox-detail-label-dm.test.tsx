// @vitest-environment jsdom

import { describe, expect, it, vi } from "vitest";
import type { InboxItem } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { InboxDetailLabel, useTypeLabels } from "./inbox-detail-label";

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: () => "Someone" }),
}));
vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () => ({
    categoryOf: () => null,
    colorOf: () => null,
    iconOf: () => null,
  }),
}));
vi.mock("../../issues/utils/status-label", () => ({
  useStatusLabel: () => (key: string) => key,
}));

function dm(overrides: Partial<InboxItem> = {}): InboxItem {
  return {
    id: "inbox-1",
    workspace_id: "workspace-1",
    recipient_type: "member",
    recipient_id: "member-1",
    actor_type: "agent",
    actor_id: "agent-1",
    type: "dm_reply",
    severity: "info",
    issue_id: null,
    chat_session_id: "chat-1",
    title: "Deploy plan",
    body: null,
    issue_status: null,
    read: false,
    archived: false,
    created_at: "2026-07-27T08:00:00Z",
    details: null,
    ...overrides,
  };
}

const text = (item: InboxItem, locale?: "zh-Hans" | "fr") =>
  renderWithI18n(<InboxDetailLabel item={item} />, { locale }).container
    .textContent;

describe("InboxDetailLabel direct messages", () => {
  it("says New reply for one reply, a missing count or zero", () => {
    expect(text(dm({ details: { reply_count: "1" } }))).toBe("New reply");
    expect(text(dm({ details: null }))).toBe("New reply");
    expect(text(dm({ details: { reply_count: "0" } }))).toBe("New reply");
  });

  it("counts replies that arrive as a string or a number", () => {
    expect(text(dm({ details: { reply_count: "3" } }))).toBe("3 new replies");
    expect(
      text(dm({ details: { reply_count: 4 } as unknown as Record<string, string> })),
    ).toBe("4 new replies");
  });

  it("says Decision needed for any other DM-linked row", () => {
    expect(text(dm({ type: "mentioned" }))).toBe("Decision needed");
    expect(text(dm({ type: "mentioned", issue_id: "issue-1" }))).toBe(
      "Decision needed",
    );
  });

  it("does not touch rows without a DM link", () => {
    expect(text(dm({ type: "mentioned", chat_session_id: null }))).toBe("Mentioned");
  });

  it("localizes the DM lines", () => {
    expect(text(dm({ details: { reply_count: "3" } }), "zh-Hans")).not.toMatch(
      /new repl/i,
    );
    expect(text(dm({ type: "mentioned" }), "fr")).not.toBe("Decision needed");
  });

  it("labels the dm_reply type", () => {
    function Probe() {
      return <span>{useTypeLabels().dm_reply}</span>;
    }
    expect(renderWithI18n(<Probe />).container.textContent).toBeTruthy();
  });
});
