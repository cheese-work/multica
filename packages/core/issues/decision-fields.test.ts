// @vitest-environment node

import { describe, expect, it } from "vitest";
import type { TimelineEntry } from "../types";
import { keepDecisionFields, pickDecisionFields } from "./decision-fields";

describe("pickDecisionFields", () => {
  it("keeps only defined decision fields", () => {
    const comment = {
      id: "c",
      content: "quote",
      decision_answer_id: "answer",
      decision_state: "pending" as const,
      decision_source_message_id: undefined,
    };
    expect(pickDecisionFields(comment)).toEqual({ decision_answer_id: "answer", decision_state: "pending" });
  });
});

describe("receipt fields", () => {
  it("survive a comment snapshot mapping", () => {
    const comment = { decision_receipt_answer_id: "answer", decision_delivered: true, decision_chosen_option_index: 0, decision_option_label: "Hold" };
    expect(pickDecisionFields(comment)).toEqual(comment);
  });
});

const entry = (fields: Partial<TimelineEntry>): TimelineEntry => ({
  type: "comment",
  id: "c",
  actor_type: "agent",
  actor_id: "agent",
  created_at: "now",
  ...fields,
});

describe("keepDecisionFields", () => {
  const prev = {
    decision_answer_id: "answer",
    decision_state: "pending" as const,
    decision_source_session_id: "session",
    decision_source_message_id: "message",
    decision_request_session_id: "ask-session",
    decision_request_message_id: "ask-message",
  };

  it("keeps the fields a sparse snapshot lacks, including the owner-only DM pointers", () => {
    expect(keepDecisionFields(entry({}), prev)).toMatchObject(prev);
  });

  it("lets the snapshot's own values win and still keeps pointers it omits", () => {
    const next = keepDecisionFields(entry({ decision_state: "effective" }), prev);
    expect(next.decision_state).toBe("effective");
    expect(next.decision_source_message_id).toBe("message");
  });
});
