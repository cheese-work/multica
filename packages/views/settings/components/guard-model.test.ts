// @vitest-environment node
import { expect, it } from "vitest";
import type { GuardStats } from "@multica/core/types";
import { applyGuardEdit, buildGuardRows, parseGuardPolicy } from "./guard-model";

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
    plugins: [{ key: "0:0:gh-pr", scope: "workspace", target: "", layerLocked: false, name: "gh-pr", tool: "gh", subcommand: ["pr"], action: "fix", message: "", failClosed: false, builtin: "gh-pr" }],
    review: [{ scope: "workspace", target: "", repo: "cheese-work/*", github: "skip", reviewers: ["a"] }],
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

const LAYERED = {
  guard_policy: {
    version: 3,
    plugins: [{ name: "gh-pr", match: { tool: "gh" }, action: "fix" }],
    review: [{ repo: "cheese-work/*", reviewers: ["a"] }],
    layers: [
      { scope: "org", org: "cheese-work", locked: true, plugins: [{ name: "gh-pr", match: { tool: "gh" }, action: "deny" }] },
      { scope: "repo", repo: "cheese-work/multica", review: [{ repo: "cheese-work/multica", github: "skip" }] },
      { scope: "path", repo: "cheese-work/multica", path: "docs/", plugins: [{ name: "docs-only", match: { tool: "gh" }, action: "pass" }] },
    ],
  },
};

it("reads layer plugins and review rules with their scope, target and lock", () => {
  const policy = parseGuardPolicy(LAYERED)!;
  expect(policy.plugins.map((p) => [p.scope, p.target, p.layerLocked, p.name, p.action])).toEqual([
    ["workspace", "", false, "gh-pr", "fix"],
    ["org", "cheese-work", true, "gh-pr", "deny"],
    ["path", "cheese-work/multica/docs", false, "docs-only", "pass"],
  ]);
  expect(policy.review.map((r) => [r.scope, r.target, r.repo])).toEqual([
    ["workspace", "", "cheese-work/*"],
    ["repo", "cheese-work/multica", "cheese-work/multica"],
  ]);
});

it("keeps a plugin name that repeats across scopes as separate rows with unique keys", () => {
  const rows = buildGuardRows(parseGuardPolicy(LAYERED), stats);
  const ghPr = rows.filter((r) => r.name === "gh-pr");
  expect(ghPr.map((r) => r.scope)).toEqual(["workspace", "org"]);
  expect(new Set(rows.map((r) => r.key)).size).toBe(rows.length);
  expect(ghPr.every((r) => r.sharedCounts && r.fixed === 42)).toBe(true);
  expect(rows.find((r) => r.name === "docs-only")!.sharedCounts).toBe(false);
});

const STORED = {
  version: 1,
  plugins: [
    { name: "ci-wait", match: { tool: "gh" }, action: "deny", builtin: "ci-wait", message: "wait", future_field: { keep: true } },
    { name: "pr-title-tag", match: { tool: "gh" }, action: "fix", builtin: "gh-pr" },
  ],
  layers: [{ scope: "org", org: "acme", plugins: [{ name: "ci-wait", action: "deny" }] }],
  review: [{ repo: "a/b" }],
};

it("changes only the named workspace plugin's action and message and keeps every other field", () => {
  const next = applyGuardEdit(STORED, "ci-wait", { action: "pass", message: "" });
  expect(next).toEqual({
    ...STORED,
    plugins: [
      { name: "ci-wait", match: { tool: "gh" }, action: "pass", builtin: "ci-wait", future_field: { keep: true } },
      STORED.plugins[1],
    ],
  });
  expect(STORED.plugins[0]!.action).toBe("deny");
});

it("sets a non-empty message and returns null when the workspace has no such plugin", () => {
  const next = applyGuardEdit(STORED, "pr-title-tag", { action: "fix", message: "tag added" }) as typeof STORED;
  expect(next.plugins[1]).toMatchObject({ message: "tag added" });
  expect(applyGuardEdit(STORED, "gone", { action: "pass", message: "" })).toBeNull();
  expect(applyGuardEdit(null, "ci-wait", { action: "pass", message: "" })).toBeNull();
});
