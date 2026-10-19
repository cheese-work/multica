// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "../api/client";

// Callers treat an empty workflow list as the answer "this workspace stores no
// workflow", which lets a status change skip its handoff confirmation. A body
// that is not a workflow list says nothing, so it must fail, not read as empty.

afterEach(() => vi.unstubAllGlobals());

const client = new ApiClient("https://api.example.test");
const respond = (body: unknown) =>
  vi.fn().mockImplementation(async () => new Response(JSON.stringify(body), { status: 200 }));

describe("ApiClient.listIssueWorkflows", () => {
  it.each([
    ["a workflows value that is not a list", { workflows: "nope", total: 1 }],
    ["a body without a workflows list", {}],
    ["a null body", null],
    ["a list holding a malformed workflow", { workflows: [{ id: 7 }], total: 1 }],
  ])("rejects %s", async (_label, body) => {
    vi.stubGlobal("fetch", respond(body));

    await expect(client.listIssueWorkflows()).rejects.toThrow();
  });

  it("accepts a genuinely empty list", async () => {
    vi.stubGlobal("fetch", respond({ workflows: [], total: 0 }));

    await expect(client.listIssueWorkflows()).resolves.toMatchObject({ workflows: [] });
  });
});
