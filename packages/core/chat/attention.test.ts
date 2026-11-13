import { describe, expect, it } from "vitest";
import { parseLaterDuration, LATER_MAX_SECONDS, isDecisionKey, snoozableKeys } from "./attention";
import { parseChatCommandInput, CHAT_COMMANDS_BY_ID } from "./commands";

describe("parseLaterDuration", () => {
  it("returns undefined seconds for an empty argument (server default 2 h)", () => {
    expect(parseLaterDuration("")).toEqual({ ok: true, seconds: undefined });
    expect(parseLaterDuration("   ")).toEqual({ ok: true, seconds: undefined });
  });
  it("parses s, m, h, d units", () => {
    expect(parseLaterDuration("90s")).toEqual({ ok: true, seconds: 90 });
    expect(parseLaterDuration("30m")).toEqual({ ok: true, seconds: 1800 });
    expect(parseLaterDuration("2h")).toEqual({ ok: true, seconds: 7200 });
    expect(parseLaterDuration("1d")).toEqual({ ok: true, seconds: 86400 });
    expect(parseLaterDuration(" 4H ")).toEqual({ ok: true, seconds: 14400 });
  });
  it("rejects garbage, zero and over 7 days", () => {
    expect(parseLaterDuration("soon")).toEqual({ ok: false });
    expect(parseLaterDuration("0m")).toEqual({ ok: false });
    expect(parseLaterDuration("5")).toEqual({ ok: false });
    expect(parseLaterDuration("8d")).toEqual({ ok: false });
    expect(parseLaterDuration("7d")).toEqual({ ok: true, seconds: LATER_MAX_SECONDS });
  });
});

describe("/later command", () => {
  it("is a client command with an optional argument", () => {
    expect(CHAT_COMMANDS_BY_ID.later).toMatchObject({ kind: "client", args: "optional_text" });
  });
  it("parses with and without a duration, rejects attachments", () => {
    expect(parseChatCommandInput("/later")).toMatchObject({ status: "ok", args: "" });
    expect(parseChatCommandInput("/later 2h")).toMatchObject({ status: "ok", args: "2h" });
    expect(parseChatCommandInput("/later", { hasAttachments: true })).toMatchObject({
      status: "invalid",
      reason: "no_attachments",
    });
  });
  it("does not capture ordinary words that start with later", () => {
    expect(parseChatCommandInput("/laterz")).toBeNull();
  });
});

describe("subject keys", () => {
  it("detects decision keys and drops them from snooze/ack targets", () => {
    expect(isDecisionKey("decision:abc@2")).toBe(true);
    expect(isDecisionKey("item:abc")).toBe(false);
    expect(snoozableKeys(["item:1", "decision:2@1", "turn:3"])).toEqual(["item:1", "turn:3"]);
  });
});
