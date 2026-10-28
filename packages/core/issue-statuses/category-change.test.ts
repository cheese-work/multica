// @vitest-environment node
import { describe, expect, it } from "vitest";
import { ApiError } from "../api/client";
import { parseIssueStatusCategoryChangeConflict } from "./category-change";

const conflictBody = {
  code: "category_change_requires_confirmation",
  issue_count: 3,
  from_category: "done",
  to_category: "started",
};

describe("category change conflict", () => {
  it("parses the count and the server's source and target categories", () => {
    const conflict = new ApiError("confirm", 409, "Conflict", conflictBody);
    expect(parseIssueStatusCategoryChangeConflict(conflict)).toEqual({ issueCount: 3, from: "done", to: "started" });
  });

  it("normalizes legacy category names", () => {
    const conflict = new ApiError("confirm", 409, "Conflict", { ...conflictBody, from_category: "canceled", to_category: "completed" });
    expect(parseIssueStatusCategoryChangeConflict(conflict)).toEqual({ issueCount: 3, from: "closed", to: "done" });
  });

  it("rejects anything but the structured confirmation conflict", () => {
    for (const body of [
      null,
      {},
      { ...conflictBody, code: "issue_status_in_use" },
      { ...conflictBody, issue_count: "3" },
      { ...conflictBody, from_category: "nonsense" },
      { code: conflictBody.code, issue_count: 3 },
    ]) {
      expect(parseIssueStatusCategoryChangeConflict(new ApiError("x", 409, "Conflict", body))).toBeNull();
    }
    expect(parseIssueStatusCategoryChangeConflict(new ApiError("x", 500, "Err", conflictBody))).toBeNull();
    expect(parseIssueStatusCategoryChangeConflict(new Error("offline"))).toBeNull();
  });
});
