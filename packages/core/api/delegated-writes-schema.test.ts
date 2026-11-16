// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";
import { DelegatedWritesSchema } from "./schemas";

describe("DelegatedWritesSchema", () => {
  it("passes a well-formed rule through", () => {
    const rule = { allowed_agents: ["a1"], allowed_fields: ["agent.instructions"], requires_human_decision: false };
    expect(DelegatedWritesSchema.parse(rule)).toMatchObject(rule);
  });

  it("defaults null/missing lists to empty and a bad switch to the safe true", () => {
    expect(
      DelegatedWritesSchema.parse({ allowed_agents: null, requires_human_decision: "nope" }),
    ).toMatchObject({ allowed_agents: [], allowed_fields: [], requires_human_decision: true });
  });
});

describe("ApiClient.updateDelegatedWrites", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("parses the PUT response through the schema (missing lists become empty)", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(
      JSON.stringify({ requires_human_decision: false }),
      { status: 200, headers: { "Content-Type": "application/json" } },
    )));
    const sent = { allowed_agents: ["a1"], allowed_fields: ["agent.instructions"], requires_human_decision: false };
    const out = await new ApiClient("https://api.example.test").updateDelegatedWrites("ws", sent);
    expect(out).toMatchObject({ allowed_agents: [], allowed_fields: [], requires_human_decision: false });
  });
});
