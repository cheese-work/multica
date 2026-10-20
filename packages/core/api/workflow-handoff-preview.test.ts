// @vitest-environment node

import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

afterEach(() => vi.unstubAllGlobals());

function stubFetch(body: unknown) {
  const fetchMock = vi.fn().mockResolvedValue(
    new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }),
  );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

const preview = () => new ApiClient("https://api.example.test").previewWorkflowHandoff("issue-1", "in_review");

// The preview is what asks the person to confirm a handoff. A successful
// response the client cannot read must not turn into "nothing to confirm": the
// dialog applies a no-handoff preview on its own, and the server then applies
// the workflow's handoff with no confirmation shown.
describe("previewWorkflowHandoff", () => {
  it.each([
    ["a wrong-typed handoff flag", { handoff: "yes", previous_runs: 3 }],
    ["a missing handoff flag", { workflow_name: "Delivery", to_status: "in_review" }],
    ["an empty object", {}],
    ["null", null],
    ["a list", []],
  ])("rejects %s instead of reading it as no handoff", async (_name, body) => {
    stubFetch(body);
    await expect(preview()).rejects.toThrow();
  });

  it("reads a valid response without a handoff as no handoff", async () => {
    stubFetch({ handoff: false });
    const result = await preview();
    expect(result.handoff).toBe(false);
  });

  it("reads a valid handoff response with its handler", async () => {
    stubFetch({
      handoff: true,
      workflow_name: "Delivery",
      to_status: "in_review",
      handler_type: "agent",
      handler_id: "agent-1",
      previous_runs: [{ task_id: "t1", agent_id: "agent-0", status: "running", started_at: null }],
    });
    const result = await preview();
    expect(result.handoff).toBe(true);
    expect(result.handler_id).toBe("agent-1");
    expect(result.previous_runs).toHaveLength(1);
  });
});
