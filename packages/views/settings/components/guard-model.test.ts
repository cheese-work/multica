// @vitest-environment node
import { expect, it } from "vitest";
import type { GuardStats } from "@multica/core/types";
import { buildGuardRows, parseGuardPolicy } from "./guard-model";

const stats: GuardStats = {
  days: 7,
  tz: "UTC",
  since: "2026-11-08",
  plugins: [
    { plugin: "gh-pr", fixed: 42, refused: 3, errors: 1, shadow_hits: 0, passes: 9, daily: [] },
    { plugin: "wakeup-create", fixed: 0, refused: 0, errors: 0, shadow_hits: 5, passes: 0, daily: [] },
  ],
};

it("reads no stored policy as null and a malformed one as null", () => {
  expect(parseGuardPolicy({})).toBeNull();
  expect(parseGuardPolicy({ guard_policy: "x" })).toBeNull();
  expect(parseGuardPolicy({ guard_policy: null })).toBeNull();
});

it("keeps what the page shows and drops unknown fields", () => {
  const policy = parseGuardPolicy({
    guard_policy: {
      version: 1,
      locked: true,
      plugins: [{ name: "gh-pr", match: { tool: "gh", subcommand: ["pr"] }, action: "fix", builtin: "gh-pr", extra: 1 }],
      review: [{ repo: "cheese-work/*", github: "skip", reviewers: ["a"] }],
    },
  });
  expect(policy).toEqual({
    version: 1,
    locked: true,
    plugins: [{ name: "gh-pr", tool: "gh", subcommand: ["pr"], action: "fix", message: "", failClosed: false, builtin: "gh-pr" }],
    review: [{ repo: "cheese-work/*", github: "skip", reviewers: ["a"] }],
  });
});

it("joins counts to policy plugins and lists a plugin that only has hits", () => {
  const policy = parseGuardPolicy({ guard_policy: { version: 1, plugins: [{ name: "gh-pr", match: { tool: "gh" }, action: "deny", message: "no" }] } });
  const rows = buildGuardRows(policy, stats);
  expect(rows.map((r) => r.name)).toEqual(["gh-pr", "wakeup-create"]);
  expect(rows[0]).toMatchObject({ action: "deny", inPolicy: true, fixed: 42, refused: 3, errors: 1 });
  expect(rows[1]).toMatchObject({ action: "unknown", inPolicy: false, shadowHits: 5 });
});

it("lists plugins that have no hits yet with zero counts", () => {
  const policy = parseGuardPolicy({ guard_policy: { version: 1, plugins: [{ name: "quiet", match: { tool: "gh" }, action: "pass" }] } });
  expect(buildGuardRows(policy, { ...stats, plugins: [] })[0]).toMatchObject({ name: "quiet", fixed: 0, refused: 0, errors: 0 });
});
