// @vitest-environment node
import { afterEach, expect, it, vi } from "vitest";
import { ApiClient } from "./client";
import { WakeupDefinitionListSchema, WakeupEffectiveRuleSchema } from "./schemas";

afterEach(() => vi.unstubAllGlobals());
const client = new ApiClient("https://api.example.test");
const respond = (body: unknown, status = 200) =>
  vi.fn().mockImplementation(async () => new Response(JSON.stringify(body), { status }));

const definition = {
  scope: "project",
  scope_id: "p1",
  rule_key: "child_done",
  root: false,
  revision: 2,
  config: { v: 1, name: "x", future_field: { a: 1 } },
  updated_at: null,
};
const capabilities = { definition_writes: true, trigger_kinds: ["child_done"], fields: ["name"] };

it("routes each scope to its own definitions path", async () => {
  const fetcher = respond({ definitions: [], capabilities });
  vi.stubGlobal("fetch", fetcher);
  await client.listWakeupDefinitions({ kind: "workspace" });
  await client.listWakeupDefinitions({ kind: "project", id: "p/1" });
  await client.listWakeupDefinitions({ kind: "issue", id: "i1" });
  expect(fetcher.mock.calls.map(([url]) => new URL(url).pathname)).toEqual([
    "/api/wakeup-definitions",
    "/api/projects/p%2F1/wakeup-definitions",
    "/api/issues/i1/wakeup-definitions",
  ]);
});

it("keeps fields a newer server adds to a definition config", async () => {
  vi.stubGlobal("fetch", respond({ definitions: [definition], capabilities }));
  const list = await client.listWakeupDefinitions({ kind: "project", id: "p1" });
  expect(list.definitions[0]?.config.future_field).toEqual({ a: 1 });
  expect(list.definitions[0]?.redacted).toBe(false);
});

it("treats a missing capability block as writes closed", () => {
  const parsed = WakeupDefinitionListSchema.parse({ definitions: [] });
  expect(parsed.capabilities).toEqual({ definition_writes: false, trigger_kinds: [], fields: [] });
  const bad = WakeupDefinitionListSchema.parse({ definitions: [], capabilities: { definition_writes: "yes" } });
  expect(bad.capabilities.definition_writes).toBe(false);
});

it("does not present a malformed definition list as empty", async () => {
  vi.stubGlobal("fetch", respond({ definitions: [{ ...definition, revision: "2" }], capabilities }));
  await expect(client.listWakeupDefinitions({ kind: "workspace" })).rejects.toThrow("Could not load wakeup definitions");
});

it("does not present a malformed save response as a saved definition", async () => {
  vi.stubGlobal("fetch", respond({ rule_key: "child_done" }));
  await expect(
    client.saveWakeupDefinition({ kind: "issue", id: "i1" }, "child_done", { revision: 0, config: { v: 1, name: "x" } }),
  ).rejects.toThrow("Could not read the saved wakeup definition");
});

it("surfaces a revision conflict instead of swallowing it", async () => {
  vi.stubGlobal("fetch", respond({ error: "wakeup changed; refresh and retry" }, 409));
  await expect(
    client.saveWakeupDefinition({ kind: "issue", id: "i1" }, "child_done", { revision: 1, config: { v: 1, name: "x" } }),
  ).rejects.toThrow();
});

it("sends the observed revision on save, create and delete", async () => {
  const fetcher = vi.fn().mockImplementation(async () => new Response(JSON.stringify(definition)));
  vi.stubGlobal("fetch", fetcher);
  await client.saveWakeupDefinition({ kind: "project", id: "p1" }, "child_done", { revision: 2, config: { v: 1, name: "x" } });
  await client.createWakeupDefinition({ kind: "workspace" }, { config: { v: 1, trigger: { kind: "pr_merged" }, instruction: "go" } });
  expect(JSON.parse(fetcher.mock.calls[0]?.[1].body)).toMatchObject({ revision: 2, config: { name: "x" } });
  expect(JSON.parse(fetcher.mock.calls[1]?.[1].body)).toMatchObject({ revision: 0 });
  fetcher.mockImplementation(async () => new Response(null, { status: 204 }));
  await client.deleteWakeupDefinition({ kind: "project", id: "p1" }, "child_done", 3);
  expect(new URL(fetcher.mock.calls[2]?.[0]).search).toBe("?revision=3");
});

const effective = {
  rule_key: "child_done",
  scope: "issue",
  applicable: true,
  enabled: true,
  config: { v: 1 },
  sources: { enabled: "builtin" },
  overrides: [],
  aggregate_caps: [],
  fingerprint: "abc",
  execution: null,
  capabilities,
};

it("reads an effective rule and defaults what an older server omits", () => {
  const parsed = WakeupEffectiveRuleSchema.parse({ ...effective, sources: undefined, overrides: undefined, redacted: undefined });
  expect(parsed).toMatchObject({ sources: {}, overrides: [], redacted: false, inapplicable_reason: "", execution: null });
});

it("does not present a malformed effective rule as resolved", async () => {
  vi.stubGlobal("fetch", respond({ ...effective, enabled: "true" }));
  await expect(client.getEffectiveWakeupRule({ kind: "issue", id: "i1" }, "child_done")).rejects.toThrow(
    "Could not load the effective wakeup rule",
  );
  vi.stubGlobal("fetch", respond({ rule_key: "x" }));
  await expect(
    client.previewWakeupDefinition({ kind: "workspace" }, { rule_key: "", config: { v: 1 } }),
  ).rejects.toThrow("Could not preview the wakeup rule");
});
