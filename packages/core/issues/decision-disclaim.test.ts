// @vitest-environment node

import { describe, expect, it } from "vitest";
import { ApiError } from "../api/client";
import { disclaimConflict, splitDecisionQuote } from "./decision-disclaim";

describe("disclaimConflict", () => {
  it("names the two 409 answers of the Disclaim endpoint", () => {
    expect(disclaimConflict(new ApiError("decision context changed; retry", 409, "Conflict"))).toBe("changed");
    expect(disclaimConflict(new ApiError("decision answer is already withdrawn", 409, "Conflict"))).toBe("withdrawn");
  });

  it("returns undefined for every other failure", () => {
    expect(disclaimConflict(new ApiError("only the request's human can disclaim", 403, "Forbidden"))).toBeUndefined();
    expect(disclaimConflict(new ApiError("something else", 409, "Conflict"))).toBe("changed");
    expect(disclaimConflict(new Error("network"))).toBeUndefined();
    expect(disclaimConflict(undefined)).toBeUndefined();
  });
});

describe("splitDecisionQuote", () => {
  it("separates the agent's note from the quoted human words", () => {
    expect(splitDecisionQuote("Agent note.\n\n> Yes.\n> Keep the words.")).toEqual({
      note: "Agent note.",
      quote: "Yes.\nKeep the words.",
    });
  });

  it("handles a quote with no note and keeps blank quote lines", () => {
    expect(splitDecisionQuote("> One\n>\n> Two")).toEqual({ note: "", quote: "One\n\nTwo" });
  });

  it("treats content with no blockquote as a note only", () => {
    expect(splitDecisionQuote("just text")).toEqual({ note: "just text", quote: "" });
  });
});
