import { describe, expect, it } from "vitest";
import { EMPTY_WORKFLOW_LINT, WorkflowLintResponseSchema } from "./schemas";
import { parseWithFallback } from "./schema";

const opts = { endpoint: "GET /api/issue-workflows/{id}/lint" };

describe("WorkflowLintResponseSchema", () => {
  it("keeps a well-formed lint response", () => {
    const body = {
      workflow_id: "wf-1",
      workflow: "Delivery",
      findings: [{ rule: "WF003", level: "error", step: "done", message: "m", hint: "h", waived: false, waive_reason: "" }],
      errors: 1,
      warnings: 0,
      infos: 0,
    };
    expect(parseWithFallback(body, WorkflowLintResponseSchema, EMPTY_WORKFLOW_LINT, opts)).toMatchObject(body);
  });

  it("falls back to an empty result when the body is malformed", () => {
    expect(parseWithFallback({ findings: "nope" }, WorkflowLintResponseSchema, EMPTY_WORKFLOW_LINT, opts)).toEqual(
      EMPTY_WORKFLOW_LINT,
    );
    expect(parseWithFallback(null, WorkflowLintResponseSchema, EMPTY_WORKFLOW_LINT, opts)).toEqual(EMPTY_WORKFLOW_LINT);
  });

  it("fills missing counts and reads an unknown level as a note", () => {
    const parsed = parseWithFallback(
      { findings: [{ rule: "WF999", level: "fatal", message: "m" }] },
      WorkflowLintResponseSchema,
      EMPTY_WORKFLOW_LINT,
      opts,
    );
    expect(parsed.errors).toBe(0);
    expect(parsed.findings[0]!.level).toBe("info");
  });
});
