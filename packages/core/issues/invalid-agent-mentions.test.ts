import { describe, expect, it } from "vitest";
import { ApiError } from "../api/client";
import { invalidAgentMentionRanges, invalidAgentMentions } from "./invalid-agent-mentions";

const body = (invalid_mentions: unknown) => ({ error: "nope", code: "invalid_agent_mentions", invalid_mentions });

describe("invalidAgentMentionRanges", () => {
  it("returns the byte ranges of a 422 invalid_agent_mentions error", () => {
    const err = new ApiError("nope", 422, "Unprocessable Entity", body([{ start: 4, end: 20 }]));
    expect(invalidAgentMentionRanges(err)).toEqual([{ start: 4, end: 20 }]);
  });

  it("drops malformed ranges and keeps the code match", () => {
    const err = new ApiError("nope", 422, "Unprocessable Entity", body([{ start: 1, end: 2 }, { start: "x" }, null, { start: 5, end: 3 }]));
    expect(invalidAgentMentionRanges(err)).toEqual([{ start: 1, end: 2 }]);
  });

  it("returns [] when the code matches but the ranges are missing (older/odd body)", () => {
    expect(invalidAgentMentionRanges(new ApiError("x", 422, "", body(undefined)))).toEqual([]);
  });

  it("returns undefined for other codes, other statuses and non-ApiErrors", () => {
    expect(invalidAgentMentionRanges(new ApiError("x", 422, "", { code: "other" }))).toBeUndefined();
    expect(invalidAgentMentionRanges(new ApiError("x", 400, "", body([])))).toBeUndefined();
    expect(invalidAgentMentionRanges(new Error("boom"))).toBeUndefined();
  });
});

describe("invalidAgentMentions", () => {
  it("slices the mentions from the user's own content by byte offset (multi-byte safe)", () => {
    const content = "héllo [@Ghost](mention://agent/abc) and [@Bob](mention://agent/def)";
    const enc = new TextEncoder();
    const first = "[@Ghost](mention://agent/abc)";
    const start = enc.encode("héllo ").length;
    const ranges = [{ start, end: start + enc.encode(first).length }];
    expect(invalidAgentMentions(content, ranges)).toEqual([{ label: "Ghost", type: "agent", id: "abc" }]);
  });

  it("skips out-of-range or non-mention slices", () => {
    expect(invalidAgentMentions("plain", [{ start: 0, end: 99 }, { start: 0, end: 3 }])).toEqual([]);
  });
});
