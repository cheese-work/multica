import { describe, expect, it } from "vitest";

import { classifyIssueLoadError } from "./issue-load-error";
import { SchemaValidationError } from "./parse-response";

describe("classifyIssueLoadError", () => {
  it("separates unreadable data, a missing issue and everything else", () => {
    expect(classifyIssueLoadError(new SchemaValidationError("getIssue", ["metadata.x"]))).toBe("data");
    expect(classifyIssueLoadError(Object.assign(new Error("gone"), { status: 404 }))).toBe("not_found");
    expect(classifyIssueLoadError(Object.assign(new Error("boom"), { status: 500 }))).toBe("network");
    expect(classifyIssueLoadError(new TypeError("Network request failed"))).toBe("network");
    expect(classifyIssueLoadError(null)).toBe("not_found");
  });
});
