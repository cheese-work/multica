import { describe, expect, it } from "vitest";
import {
  pullRequestOrderGroup,
  sortPullRequestsForDisplay,
  type PullRequestOrderInput,
} from "./pull-request-status";

type Pr = PullRequestOrderInput & { id: string };

const base: { snapshot_available: true; pr_updated_at: string } = { snapshot_available: true, pr_updated_at: "2026-11-20T00:00:00Z" };
const red = (id: string, at = base.pr_updated_at): Pr => ({
  ...base, id, state: "open", checks_rollup: "failure", checks_failed: 1, checks_total: 2, pr_updated_at: at,
});
const green = (id: string, at = base.pr_updated_at): Pr => ({
  ...base, id, state: "open", checks_rollup: "success", checks_total: 2, checks_passed: 2, pr_updated_at: at,
});
const pending = (id: string, at = base.pr_updated_at): Pr => ({
  ...base, id, state: "open", checks_rollup: "pending", checks_total: 2, pr_updated_at: at,
});
const draft = (id: string, at = base.pr_updated_at): Pr => ({
  ...base, id, state: "draft", checks_rollup: "failure", checks_failed: 1, pr_updated_at: at,
});
const merged = (id: string, at = base.pr_updated_at): Pr => ({ ...base, id, state: "merged", pr_updated_at: at });
const closed = (id: string, at = base.pr_updated_at): Pr => ({ ...base, id, state: "closed", pr_updated_at: at });

describe("pullRequestOrderGroup", () => {
  it("ranks the six groups: red, green, pending/none, draft, merged, closed", () => {
    expect(pullRequestOrderGroup(red("a"))).toBe(1);
    expect(pullRequestOrderGroup(green("a"))).toBe(2);
    expect(pullRequestOrderGroup(pending("a"))).toBe(3);
    expect(pullRequestOrderGroup(draft("a"))).toBe(4);
    expect(pullRequestOrderGroup(merged("a"))).toBe(5);
    expect(pullRequestOrderGroup(closed("a"))).toBe(6);
  });

  it("puts an open PR with no check result into the pending/none group", () => {
    expect(pullRequestOrderGroup({ state: "open", pr_updated_at: base.pr_updated_at })).toBe(3);
    expect(
      pullRequestOrderGroup({ state: "open", snapshot_available: true, checks_rollup: null, pr_updated_at: base.pr_updated_at }),
    ).toBe(3);
  });

  it("keeps a draft with failing checks in the draft group, not red", () => {
    expect(pullRequestOrderGroup(draft("a"))).toBe(4);
  });
});

describe("sortPullRequestsForDisplay", () => {
  it("orders groups red > green > pending > draft > merged > closed", () => {
    const input = [closed("closed"), merged("merged"), draft("draft"), pending("pending"), green("green"), red("red")];
    expect(sortPullRequestsForDisplay(input).map((p) => p.id)).toEqual([
      "red", "green", "pending", "draft", "merged", "closed",
    ]);
  });

  it("orders inside a group by most recently updated first", () => {
    const input = [
      red("old", "2026-11-01T00:00:00Z"),
      red("new", "2026-11-22T00:00:00Z"),
      red("mid", "2026-11-10T00:00:00Z"),
      merged("m-old", "2026-10-01T00:00:00Z"),
      merged("m-new", "2026-11-02T00:00:00Z"),
    ];
    expect(sortPullRequestsForDisplay(input).map((p) => p.id)).toEqual(["new", "mid", "old", "m-new", "m-old"]);
  });

  it("is stable for equal groups and timestamps, and does not mutate the input", () => {
    const input = [green("a"), green("b"), red("c")];
    const snapshot = [...input];
    expect(sortPullRequestsForDisplay(input).map((p) => p.id)).toEqual(["c", "a", "b"]);
    expect(input).toEqual(snapshot);
  });
});
