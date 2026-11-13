// @vitest-environment node
import { describe, expect, it, vi } from "vitest";
import { ApiError } from "@multica/core/api";

const toastError = vi.hoisted(() => vi.fn());
vi.mock("sonner", () => ({ toast: { error: toastError } }));

import { rejectInvalidAgentMentions } from "./invalid-agent-mentions-copy";
import en from "../locales/en/issues.json";

type Leaf = string | Record<string, unknown>;
// Resolves the selector against the real en bundle, interpolating {{vars}}.
const t = ((selector: (bundle: unknown) => Leaf, vars?: Record<string, unknown>) => {
  // Plural keys are stored as `<key>_one/_other`; resolve the base key like i18next.
  const comment = new Proxy(en.comment as Record<string, Leaf>, {
    get: (o, k: string) => o[k] ?? o[`${k}_${vars?.count === 1 ? "one" : "other"}`],
  });
  const value = selector({ comment });
  if (typeof value !== "string") throw new Error("selector did not resolve to a string");
  return value.replace(/\{\{(\w+)\}\}/g, (_, k) => String(vars?.[k]));
}) as Parameters<typeof rejectInvalidAgentMentions>[2];

const content = "hi [@Ghost](mention://agent/abc) and [@Bob](mention://agent/def)";
const enc = new TextEncoder();
const range = (md: string) => {
  const start = enc.encode(content.slice(0, content.indexOf(md))).length;
  return { start, end: start + enc.encode(md).length };
};
const err = (ranges: unknown) =>
  new ApiError("nope", 422, "Unprocessable Entity", { code: "invalid_agent_mentions", invalid_mentions: ranges });

describe("rejectInvalidAgentMentions", () => {
  it("returns undefined and stays silent for any other error", () => {
    toastError.mockClear();
    expect(rejectInvalidAgentMentions(new Error("boom"), content, t)).toBeUndefined();
    expect(toastError).not.toHaveBeenCalled();
  });

  it("names a single refused mention in the toast and returns its id to flag", () => {
    toastError.mockClear();
    const r = rejectInvalidAgentMentions(err([range("[@Ghost](mention://agent/abc)")]), content, t);
    expect(r).toEqual({ invalidMentionIds: ["abc"] });
    expect(toastError).toHaveBeenCalledWith(expect.stringContaining("@Ghost"));
    expect(toastError.mock.calls[0]![0]).toMatch(/draft/i);
  });

  it("falls back to a count for several mentions, and to a bare message when ranges do not resolve", () => {
    toastError.mockClear();
    const both = [range("[@Ghost](mention://agent/abc)"), range("[@Bob](mention://agent/def)")];
    expect(rejectInvalidAgentMentions(err(both), content, t)).toEqual({ invalidMentionIds: ["abc", "def"] });
    expect(toastError).toHaveBeenLastCalledWith(expect.stringContaining("2"));
    expect(rejectInvalidAgentMentions(err([{ start: 0, end: 9999 }]), content, t)).toEqual({ invalidMentionIds: [] });
    expect(toastError).toHaveBeenCalledTimes(2);
  });
});
