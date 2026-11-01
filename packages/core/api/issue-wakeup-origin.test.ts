// @vitest-environment node
import { expect, it } from "vitest";
import { IssueWakeupSchema } from "./schemas";

const base = {
  id: "w", issue_id: "i", agent_id: "a", instruction: "x", kind: "event", mode: "continuous",
  event_types: [], filter_agent_id: null, filter_task_id: null, interval_seconds: null,
  cron_expression: null, timezone: "UTC", next_fire_at: null, enabled: false, disabled_at: null,
  last_task_id: null, last_error: null,
};

it("reads the origin of a default-derived wakeup and defaults it for older servers", () => {
  expect(IssueWakeupSchema.parse({ ...base, default_rule_key: "rule-1" }).default_rule_key).toBe("rule-1");
  expect(IssueWakeupSchema.parse({ ...base, default_rule_key: null }).default_rule_key).toBeNull();
  // An older server omits the field; a malformed one reads as a local rule.
  expect(IssueWakeupSchema.parse(base).default_rule_key).toBeUndefined();
  expect(IssueWakeupSchema.parse({ ...base, default_rule_key: 7 }).default_rule_key).toBeNull();
});
