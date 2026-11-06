import { describe, expect, it } from "vitest";
import type { ChatSupplement } from "@multica/core/types";

import { presentSupplement } from "./chat-supplement";

const base: ChatSupplement = {
  message_id: "m1",
  chat_session_id: "s1",
  task_id: "t1",
  status: "pending",
  failure_reason: null,
  delivered_at: null,
  acked_after_resend_at: null,
  resent_task_id: null,
};

describe("presentSupplement", () => {
  it.each(["pending", "delivering"] as const)("shows Added to run while %s, with no action", (status) => {
    expect(presentSupplement({ ...base, status })).toEqual({
      labelKey: "supplement.added",
      action: "none",
      resent: false,
    });
  });

  it("shows Read by agent once delivered", () => {
    expect(presentSupplement({ ...base, status: "delivered" })).toMatchObject({
      labelKey: "supplement.read",
      action: "none",
    });
  });

  it("states the possible duplicate when the ack landed after a resend", () => {
    expect(
      presentSupplement({ ...base, status: "delivered", acked_after_resend_at: "2026-11-01T00:00:00Z" }),
    ).toMatchObject({ labelKey: "supplement.read_twice", action: "none" });
  });

  it("offers a plain resend for a message that was never received", () => {
    expect(presentSupplement({ ...base, status: "failed" })).toEqual({
      labelKey: "supplement.not_received",
      action: "resend",
      resent: false,
    });
  });

  it("never labels an uncertain receipt Not received and asks for confirmation before a resend", () => {
    const presented = presentSupplement({ ...base, status: "uncertain" });
    expect(presented.labelKey).toBe("supplement.unconfirmed");
    expect(presented.action).toBe("confirm_resend");
  });

  it.each(["failed", "uncertain"] as const)("offers no resend once a %s message already has a resent task", (status) => {
    expect(presentSupplement({ ...base, status, resent_task_id: "t2" })).toMatchObject({
      action: "none",
      resent: true,
    });
  });
});
