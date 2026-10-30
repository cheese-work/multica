// @vitest-environment node
import { describe, expect, it } from "vitest";
import { parseWithFallback } from "../api/schema";
import { AgentRuntimeSchema, EMPTY_RUNTIME } from "./schema";

describe("runtime sharing response contract", () => {
  it("preserves grants from a new server and defaults missing grants to empty", () => {
    const member = "11111111-1111-4111-8111-111111111111";
    expect(AgentRuntimeSchema.parse({ id: "r", workspace_id: "w", shared_with: [member] }).shared_with).toEqual([member]);
    expect(AgentRuntimeSchema.parse({ id: "r", workspace_id: "w" }).shared_with).toEqual([]);
  });
  it.each([null, "all", [123], ["not-a-uuid"]])("fails closed for malformed grants: %j", (shared_with) => {
    expect(AgentRuntimeSchema.parse({ id: "r", workspace_id: "w", shared_with }).shared_with).toEqual([]);
  });
  it("falls back safely for a malformed runtime", () => {
    expect(parseWithFallback({ id: 123 }, AgentRuntimeSchema, EMPTY_RUNTIME, { endpoint: "test" })).toEqual(EMPTY_RUNTIME);
  });

  describe("commit-based update status", () => {
    const base = { id: "r", workspace_id: "w" };

    it("passes the server's update status through", () => {
      const parsed = AgentRuntimeSchema.parse({
        ...base,
        update_available: true,
        server_commit: "0123456789abcdef0123456789abcdef01234567",
        server_version: "0.7.1-20261029-2357",
        update_reason: "",
      });
      expect(parsed).toMatchObject({
        update_available: true,
        server_commit: "0123456789abcdef0123456789abcdef01234567",
        server_version: "0.7.1-20261029-2357",
        update_reason: "",
      });
    });

    it("shows no update from an older server that omits the fields", () => {
      expect(AgentRuntimeSchema.parse(base)).toMatchObject({
        update_available: false,
        server_commit: "",
        server_version: "",
        update_reason: "",
      });
    });

    it.each([
      ["a string flag", { update_available: "yes" }],
      ["a numeric flag", { update_available: 1 }],
      ["a null flag", { update_available: null }],
    ])("fails closed (no badge) for %s", (_label, bad) => {
      expect(AgentRuntimeSchema.parse({ ...base, ...bad }).update_available).toBe(false);
    });

    it("drops malformed commit, version and reason without failing the runtime", () => {
      const parsed = AgentRuntimeSchema.parse({
        ...base,
        update_available: true,
        server_commit: 42,
        server_version: { v: 1 },
        update_reason: ["x"],
      });
      expect(parsed).toMatchObject({ server_commit: "", server_version: "", update_reason: "" });
    });
  });
});
