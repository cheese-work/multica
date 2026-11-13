// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from "vitest";
import { setCurrentWorkspace } from "../../platform/workspace-storage";
import { useDecisionAnswerDraftStore } from "./decision-answer-draft-store";

const DAY_MS = 24 * 60 * 60 * 1000;

describe("decision answer draft store", () => {
  beforeEach(() => {
    localStorage.clear();
    setCurrentWorkspace("acme", "acme-id");
  });

  it("keeps an unsent answer after a year, until it is sent or discarded", async () => {
    localStorage.setItem(
      "multica_decision_answer_drafts:acme",
      JSON.stringify({
        state: { drafts: { request: { data: { option: "Hold", text: "later" }, updatedAt: Date.now() - 365 * DAY_MS } } },
        version: 0,
      }),
    );
    // A workspace switch rehydrates the store from storage, as a reload does.
    setCurrentWorkspace("other", "other-id");
    setCurrentWorkspace("acme", "acme-id");
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(useDecisionAnswerDraftStore.getState().getDraft("request")).toEqual({ option: "Hold", text: "later" });
  });
});
