// @vitest-environment node
import { describe, expect, it } from "vitest";
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
