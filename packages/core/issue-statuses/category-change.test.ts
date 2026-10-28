// @vitest-environment node
import { describe, expect, it } from "vitest";
import { ApiError } from "../api/client";
import { issueStatusCategoryChangeConflictCount } from "./category-change";

describe("category change conflict", () => {
  it("parses only the structured confirmation conflict", () => {
    const conflict = new ApiError("confirm", 409, "Conflict", {
      code: "category_change_requires_confirmation",
      issue_count: 3,
    });
    expect(issueStatusCategoryChangeConflictCount(conflict)).toBe(3);
    for (const body of [null, {}, { code: "issue_status_in_use", issue_count: 3 }, { code: "category_change_requires_confirmation", issue_count: "3" }]) {
      expect(issueStatusCategoryChangeConflictCount(new ApiError("x", 409, "Conflict", body))).toBeNull();
    }
    expect(issueStatusCategoryChangeConflictCount(new ApiError("x", 500, "Err", conflict.body))).toBeNull();
    expect(issueStatusCategoryChangeConflictCount(new Error("offline"))).toBeNull();
  });
});
