// @vitest-environment node

import { describe, expect, it } from "vitest";
import { pickDecisionFields } from "./decision-fields";

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
