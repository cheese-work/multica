import { describe, expect, it } from "vitest";
import { CIWatchConfigSchema, CIWatchEventsSchema } from "./schema";

const config = {
  repo: "cheese-work/multica", mode: "shadow", branches: ["main"], events: ["push"],
  workflows_include: [], workflows_exclude: ["x.yml"], p1_mode: "shadow", p2_mode: "act",
  p3_mode: "shadow", p4_mode: "shadow", fix_owner_kind: "squad", fix_owner_id: "s1",
  notify_member_id: null, max_reruns_per_original_run: 1, max_new_issues_per_day: 5,
  max_owner_wakes_per_hour: 6, updated_at: "2026-11-06T01:00:00Z",
};

describe("CIWatchConfigSchema", () => {
  it("accepts the server config", () => {
    expect(CIWatchConfigSchema.safeParse(config).success).toBe(true);
    expect(CIWatchConfigSchema.parse(config)).toMatchObject({ mode: "shadow", p2_mode: "act", fix_owner_id: "s1" });
  });
  it("keeps unknown enum values instead of failing", () => {
    expect(CIWatchConfigSchema.parse({ ...config, mode: "future" }).mode).toBe("future");
  });
  it("rejects a body without the repo or with a non-numeric cap", () => {
    expect(CIWatchConfigSchema.safeParse({ ...config, repo: undefined }).success).toBe(false);
    expect(CIWatchConfigSchema.safeParse({ ...config, max_new_issues_per_day: "5" }).success).toBe(false);
  });
  it("defaults missing list fields to empty lists", () => {
    const { branches: _b, events: _e, ...rest } = config;
    expect(CIWatchConfigSchema.parse(rest)).toMatchObject({ branches: [], events: [] });
  });
});

describe("CIWatchEventsSchema", () => {
  it("parses rows and fills blank columns for breaker rows", () => {
    const rows = CIWatchEventsSchema.parse({
      events: [
        { at: "2026-11-06T01:00:00Z", kind: "ci_failure", workflow: "ci.yml", branch: "main", run_id: 7, detail: "", class: "flaky", protocol: "P1", action: "rerun", state: "done", outcome: "rerun_green", would_do: "" },
        { at: "2026-11-06T02:00:00Z", kind: "breaker_open", detail: "cap hit" },
      ],
    });
    expect(rows).toHaveLength(2);
    expect(rows[0]).toMatchObject({ class: "flaky", run_id: 7, outcome: "rerun_green" });
    expect(rows[1]).toMatchObject({ workflow: "", run_id: 0, class: "", would_do: "" });
  });
  it("falls back to no rows when the body is malformed", () => {
    expect(CIWatchEventsSchema.safeParse({ events: "nope" }).success).toBe(false);
  });
});
