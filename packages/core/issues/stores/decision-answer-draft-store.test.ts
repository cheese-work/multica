// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from "vitest";
import { currentDraftOrigin, resetAllRegisteredDrafts } from "../../drafts/cleanup-registry";
import { setCurrentWorkspace } from "../../platform/workspace-storage";
import {
  appendDecisionAttachment,
  decisionAnswerDraftKey,
  useDecisionAnswerDraftStore,
} from "./decision-answer-draft-store";

const STORAGE = "multica_decision_answer_drafts:acme";
const KEY = decisionAnswerDraftKey("r1");
const flush = () => new Promise((resolve) => queueMicrotask(() => resolve(null)));

describe("appendDecisionAttachment", () => {
  beforeEach(async () => {
    window.localStorage.clear();
    resetAllRegisteredDrafts();
    setCurrentWorkspace("acme", "ws-a");
    await flush();
  });

  it("adds the link to the stored note", () => {
    useDecisionAnswerDraftStore.getState().setDraft(KEY, { text: "see" });
    appendDecisionAttachment(currentDraftOrigin("acme"), "r1", "![a](u)");
    expect(useDecisionAnswerDraftStore.getState().getDraft(KEY)?.text).toBe("see\n\n![a](u)");
  });

  it("adds the link when no note was typed yet", () => {
    appendDecisionAttachment(currentDraftOrigin("acme"), "r1", "![a](u)");
    expect(useDecisionAnswerDraftStore.getState().getDraft(KEY)?.text).toBe("![a](u)");
  });

  // Another tab logged out and wiped shared storage. This tab never saw it: its own memory still
  // holds the note and its cleanup generation did not change.
  it("drops the write, and this tab's copy of the note, when another tab already cleared storage", () => {
    useDecisionAnswerDraftStore.getState().setDraft(KEY, { text: "private note" });
    const origin = currentDraftOrigin("acme");
    window.localStorage.removeItem(STORAGE);
    appendDecisionAttachment(origin, "r1", "![a](u)");
    expect(useDecisionAnswerDraftStore.getState().getDraft(KEY)).toBeUndefined();
    expect(window.localStorage.getItem(STORAGE) ?? "").not.toContain("private note");
    expect(window.localStorage.getItem(STORAGE) ?? "").not.toContain("![a](u)");
  });
});
