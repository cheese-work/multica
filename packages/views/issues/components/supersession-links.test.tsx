import { describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import type { TimelineEntry } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: () => "Ada" }),
}));

import { ResolvedThreadBar } from "./resolved-thread-bar";
import {
  SupersedesLine,
  SupersessionRevisionsProvider,
} from "./supersession-links";

function entry(id: string, extra: Partial<TimelineEntry> = {}): TimelineEntry {
  return {
    type: "comment",
    id,
    actor_type: "agent",
    actor_id: "agent-1",
    actor_name: "Plan Bot",
    content: `body ${id}`,
    parent_id: null,
    comment_type: "comment",
    created_at: "2026-10-30T08:00:00Z",
    ...extra,
  };
}

const chain = [
  entry("plan-1", { superseded_by_id: "plan-2", resolved_at: "2026-10-30T09:00:00Z" }),
  entry("plan-2", { supersedes: [{ comment_id: "plan-1", resolved: true }] }),
];

function renderBar(item: TimelineEntry) {
  return renderWithI18n(
    <SupersessionRevisionsProvider entries={chain}>
      <ResolvedThreadBar entry={item} replies={[]} onExpand={vi.fn()} />
    </SupersessionRevisionsProvider>,
  );
}

describe("supersession links", () => {
  it("links a resolved superseded thread to the revision that replaced it", () => {
    renderBar(chain[0]!);
    const link = screen.getByRole("link", { name: /Superseded by rev-2/ });
    expect(link).toHaveAttribute("href", "#comment-plan-2");
  });

  it("shows no link on a resolved thread that nothing superseded", () => {
    renderBar(entry("plain", { resolved_at: "2026-10-30T09:00:00Z" }));
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("shows no link when the superseding comment is not loaded", () => {
    renderBar(entry("plan-1", { superseded_by_id: "missing", resolved_at: "2026-10-30T09:00:00Z" }));
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("labels the new comment with the revision it supersedes", () => {
    renderWithI18n(
      <SupersessionRevisionsProvider entries={chain}>
        <SupersedesLine entry={chain[1]!} />
      </SupersessionRevisionsProvider>,
    );
    const link = screen.getByRole("link", { name: /Supersedes rev-1/ });
    expect(link).toHaveAttribute("href", "#comment-plan-1");
  });

  it("renders nothing for a comment that supersedes nothing", () => {
    const { container } = renderWithI18n(
      <SupersessionRevisionsProvider entries={chain}>
        <SupersedesLine entry={chain[0]!} />
      </SupersessionRevisionsProvider>,
    );
    expect(container).toBeEmptyDOMElement();
  });
});
