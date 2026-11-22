// @vitest-environment node
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { EDITABLE_GUARDS, guardNeedsTimeout, isValidGuard } from "./graph-edit";

// The Go side (`server/internal/issueworkflow/guard_cases_contract_test.go`)
// reads the same file. A guard the server accepts but this editor rejects shows
// as `bad_guard` and keeps Save disabled (D1: `webhook(...)`, CHE-6638).
interface GuardCase {
  expr: string;
  valid: boolean;
  needs_timeout: boolean;
  entry_only: boolean;
}

const cases: GuardCase[] = JSON.parse(
  readFileSync(
    new URL("../../../server/internal/issueworkflow/testdata/guard_cases.json", import.meta.url),
    "utf8",
  ),
);

describe("guard cases shared with the Go parser", () => {
  it.each(cases.map((c) => [c.expr, c] as const))(
    "isValidGuard(%j) accepts exactly the valid, non-entry-only guards",
    (_expr, c) => {
      expect(isValidGuard(c.expr)).toBe(c.valid && !c.entry_only);
    },
  );

  it.each(cases.filter((c) => c.valid && !c.entry_only).map((c) => [c.expr, c] as const))(
    "guardNeedsTimeout(%j) matches needs_timeout",
    (_expr, c) => {
      expect(guardNeedsTimeout(c.expr)).toBe(c.needs_timeout);
    },
  );

  it("every EDITABLE_GUARDS expression is a valid, non-entry-only case", () => {
    expect(strayEditableGuards(EDITABLE_GUARDS, cases)).toEqual([]);
  });

  it("a drifted EDITABLE_GUARDS list fails and names the guard and the fix", () => {
    const stray = strayEditableGuards([...EDITABLE_GUARDS, "pr.none_open", "pr.checks_blue"], cases);
    expect(stray).toEqual([
      expect.stringContaining('"pr.none_open"'),
      expect.stringContaining('"pr.checks_blue"'),
    ]);
    expect(stray[0]).toContain("guard_cases.json");
  });
});

function strayEditableGuards(guards: readonly string[], all: GuardCase[]): string[] {
  const byExpr = new Map(all.map((c) => [c.expr.trim(), c]));
  return guards.flatMap((g) => {
    const c = byExpr.get(g);
    return !c || !c.valid || c.entry_only
      ? [`EDITABLE_GUARDS has "${g}", which is not a valid non-entry-only case in server/internal/issueworkflow/testdata/guard_cases.json: remove it from EDITABLE_GUARDS, or fix the golden case`]
      : [];
  });
}
