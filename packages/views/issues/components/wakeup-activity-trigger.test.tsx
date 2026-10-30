import { describe, expect, it, vi } from "vitest";
import { renderHook } from "@testing-library/react";
import type { TimelineEntry } from "@multica/core/types";
import { I18nProvider } from "@multica/core/i18n/react";
import { RESOURCES } from "../../test/i18n";
import { useT } from "../../i18n";
import { useWakeupText } from "./wakeup-presentation";
import * as activity from "./wakeup-activity";

vi.mock("./wakeup-condition-names", () => ({
  useConditionNames: () => ({ status: (key: string) => key, label: () => undefined, property: () => undefined, actor: (_type: string, id: string) => id }),
}));
vi.mock("../../common/use-viewing-timezone", () => ({ useViewingTimezone: () => "UTC" }));

const names: Record<string, string> = { a: "Emacs", u: "Jiayuan" };
const getActorName = (_type: string, id: string) => names[id] ?? id;
const entry = (action: string, details: Record<string, unknown>): TimelineEntry =>
  ({ type: "activity", id: action, actor_type: "system", actor_id: "", action, details, created_at: "2026-10-15T00:00:00Z" }) as TimelineEntry;
// What the service stores for the built-in PR rules: no event types at all.
const prRule = { id: "w", kind: "event", mode: "continuous", event_types: [] as string[], agent_id: "" };
const commentRule = { id: "w", kind: "event", mode: "once", event_types: ["comment.created"], agent_id: "a", filter_actor_type: "member", filter_actor_id: "u" };
const pr = { pr_number: 252, pr_url: "https://github.com/o/r/pull/252", target_type: "agent", target_id: "a", outcome: "woke", task_id: "t1" };

function read() {
  const { result } = renderHook(() => ({ t: useT("issues").t, text: useWakeupText() }), {
    wrapper: ({ children }) => (
      <I18nProvider locale="en" resources={RESOURCES}>
        {children}
      </I18nProvider>
    ),
  });
  const { t, text } = result.current;
  return (e: TimelineEntry) => activity.formatWakeupActivity(e, t, text, getActorName);
}

describe("wakeup_triggered rows name what fired and who woke", () => {
  it("names a comment trigger and its author", () => {
    const format = read();
    expect(format(entry("wakeup_triggered", { wakeup: commentRule, events: ["comment.created"], actor_type: "member", actor_id: "u", comment_id: "c1", task_id: "t1" }))).toBe(
      "Comment by Jiayuan · woke Emacs",
    );
    expect(format(entry("wakeup_triggered", { wakeup: commentRule, events: ["comment.created"], task_id: "t1" }))).toBe("New comment · woke Emacs");
  });

  it("names a status trigger and who changed it", () => {
    const format = read();
    expect(
      format(entry("wakeup_triggered", { wakeup: { ...commentRule, event_types: ["issue.status_changed"] }, events: ["issue.status_changed"], actor_type: "member", actor_id: "u", task_id: "t1" })),
    ).toBe("Status changed by Jiayuan · woke Emacs");
  });

  it("names each PR rule with its PR number and the agent that was woken", () => {
    const format = read();
    expect(format(entry("wakeup_triggered", { wakeup: prRule, rule: "pr_merged", ...pr }))).toBe("PR #252 merged · woke Emacs");
    expect(format(entry("wakeup_triggered", { wakeup: prRule, rule: "pr_checks_failed", ...pr }))).toBe("PR #252 checks failed · woke Emacs");
    expect(format(entry("wakeup_triggered", { wakeup: prRule, rule: "pr_needs_attention", ...pr }))).toBe("PR #252 needs attention · woke Emacs");
    expect(format(entry("wakeup_triggered", { wakeup: prRule, rule: "pr_merged", ...pr, pr_number: undefined }))).toBe("PR merged · woke Emacs");
    expect(format(entry("wakeup_triggered", { wakeup: prRule, rule: "pr_checks_failed", ...pr, outcome: "merged" }))).toBe(
      "PR #252 checks failed · joined Emacs's waiting run",
    );
  });

  it("never renders an empty event name", () => {
    const format = read();
    const rows = [
      format(entry("wakeup_triggered", { wakeup: prRule, rule: "pr_merged", ...pr })),
      format(entry("wakeup_triggered", { wakeup: { ...prRule, agent_id: "a" }, task_id: "t1" })),
      format(entry("wakeup_triggered", { wakeup: { ...prRule, agent_id: "a" }, events: [], task_id: "t1" })),
    ];
    for (const row of rows) expect(row).not.toMatch(/When event/);
    expect(rows[1]).toBe(rows[2]);
    expect(rows[1]).toContain("woke Emacs");
  });

  it("does not call a member FYI a wakeup", () => {
    const format = read();
    expect(format(entry("wakeup_triggered", { wakeup: prRule, rule: "pr_needs_attention", ...pr, target_type: "member", target_id: "u", outcome: "notified" }))).toBe(
      "PR #252 needs attention · notified Jiayuan",
    );
    expect(format(entry("wakeup_triggered", { wakeup: prRule, rule: "pr_checks_failed", ...pr, target_type: "member", target_id: "u", outcome: "none" }))).toBe(
      "PR #252 checks failed",
    );
  });

  it("keeps the child_done sentence", () => {
    expect(read()(entry("wakeup_triggered", { rule: "child_done", stage: 1, total: 2, target_type: "agent", target_id: "a", outcome: "woke" }))).toBe(
      "Stage 1's 2 sub-issues all finished · woke Emacs",
    );
  });
});

describe("wakeup_skipped rows", () => {
  it("are timeline wakeup entries", () => {
    expect(activity.WAKEUP_ACTIVITY_ACTIONS.has("wakeup_skipped")).toBe(true);
  });

  it("say what was skipped and why", () => {
    const format = read();
    expect(format(entry("wakeup_skipped", { wakeup: commentRule, reason_code: "provider_failure_cooldown", reason: "provider is cooling down" }))).toBe(
      "When this issue receives a new comment (triggered by Jiayuan) · skipped waking Emacs: provider is cooling down",
    );
    expect(format(entry("wakeup_skipped", { wakeup: prRule, rule: "pr_checks_failed", reason: "provider is cooling down" }))).toBe(
      "PR checks failed · skipped waking the assignee: provider is cooling down",
    );
  });

  it("fall back to the reason code", () => {
    expect(read()(entry("wakeup_skipped", { wakeup: commentRule, reason_code: "provider_failure_cooldown" }))).toContain("provider_failure_cooldown");
  });
});

describe("links a row offers", () => {
  const links = (e: TimelineEntry) => {
    expect(typeof activity.wakeupActivityLinks).toBe("function");
    return activity.wakeupActivityLinks(e);
  };

  it("links the PR, the started run and the triggering comment", () => {
    expect(links(entry("wakeup_triggered", { wakeup: prRule, rule: "pr_merged", ...pr }))).toEqual({
      pr: { number: 252, url: "https://github.com/o/r/pull/252" },
      taskId: "t1",
    });
    expect(links(entry("wakeup_triggered", { wakeup: commentRule, events: ["comment.created"], comment_id: "c1", task_id: "t1" }))).toEqual({
      commentId: "c1",
      taskId: "t1",
    });
  });

  it("offers nothing the receipt lacks, and no unsafe PR url", () => {
    expect(links(entry("wakeup_triggered", { wakeup: commentRule, events: ["comment.created"] }))).toEqual({});
    expect(links(entry("wakeup_triggered", { wakeup: prRule, rule: "pr_merged", pr_number: 7, pr_url: "javascript:alert(1)" }))).toEqual({});
  });
});

describe("chip", () => {
  it("tags every built-in rule row as a system rule", () => {
    const { result } = renderHook(() => ({ t: useT("issues").t, text: useWakeupText() }), {
      wrapper: ({ children }) => (
        <I18nProvider locale="en" resources={RESOURCES}>
          {children}
        </I18nProvider>
      ),
    });
    const { t, text } = result.current;
    expect(activity.wakeupActivityChip(entry("wakeup_triggered", { wakeup: prRule, rule: "pr_merged", ...pr }), t, text, getActorName)).toBe("System rule");
  });
});
