import { describe, expect, it } from "vitest";
import { SchemaValidationError, parseOrThrow } from "../api/schema";
import { AttachmentRevisionDiffSchema } from "../api/schemas";
import type { Attachment } from "../types";
import { chainedIssueAttachments, parseUnifiedDiff } from "./revision-diff";

describe("parseUnifiedDiff", () => {
  it("turns a unified diff into hunks with line prefixes and start lines", () => {
    const text = [
      "--- v1",
      "+++ v2",
      "@@ -2,3 +2,3 @@",
      " keep",
      "-beta",
      "+gamma",
      " tail",
      "@@ -20,2 +20,2 @@",
      " far",
      "-old",
      "+new",
      "",
    ].join("\n");
    expect(parseUnifiedDiff(text)).toEqual([
      { old_start: 2, new_start: 2, lines: [" keep", "-beta", "+gamma", " tail"] },
      { old_start: 20, new_start: 20, lines: [" far", "-old", "+new"] },
    ]);
  });

  it("returns no hunks for an empty diff and ignores text before the first hunk", () => {
    expect(parseUnifiedDiff("")).toEqual([]);
    expect(parseUnifiedDiff("--- v1\n+++ v2\n")).toEqual([]);
  });

  it("keeps a line that starts with dashes inside a hunk", () => {
    const text = "--- v1\n+++ v2\n@@ -1,2 +1,2 @@\n--- rule\n+--- rule 2\n";
    expect(parseUnifiedDiff(text)[0]?.lines).toEqual(["--- rule", "+--- rule 2"]);
  });
});

describe("AttachmentRevisionDiffSchema", () => {
  const endpoint = "GET /api/attachments/{id}/diff";

  it("parses a text diff and a binary fallback", () => {
    const text = parseOrThrow(
      { from: 1, to: 2, binary: false, diff: "@@ -1 +1 @@\n-a\n+b\n" },
      AttachmentRevisionDiffSchema,
      { endpoint },
    );
    expect(text).toMatchObject({ from: 1, to: 2, binary: false, message: "" });
    const bin = parseOrThrow(
      { from: 1, to: 2, binary: true, message: "binary, sizes and SHA differ", from_size_bytes: 3, to_size_bytes: 4 },
      AttachmentRevisionDiffSchema,
      { endpoint },
    );
    expect(bin).toMatchObject({ binary: true, diff: "", message: "binary, sizes and SHA differ" });
  });

  it("throws on a malformed body instead of showing an empty diff", () => {
    for (const body of [null, "nope", { from: "x", to: 2, binary: false }, { from: 1, to: 2, binary: false, diff: 7 }]) {
      expect(() => parseOrThrow(body, AttachmentRevisionDiffSchema, { endpoint })).toThrow(SchemaValidationError);
    }
  });
});

describe("chainedIssueAttachments", () => {
  const att = (id: string, over: Partial<Attachment> = {}) =>
    ({ id, comment_id: null, revision: 1, revision_of: null, ...over }) as Attachment;

  it("keeps every member of an issue-level chain", () => {
    const all = [att("a1"), att("a2", { revision: 2, revision_of: "a1" })];
    expect(chainedIssueAttachments(all).map((a) => a.id)).toEqual(["a1", "a2"]);
  });

  it("drops lone uploads, comment uploads and a missing list", () => {
    const all = [
      att("solo"),
      att("c1", { comment_id: "cm" }),
      att("c2", { comment_id: "cm", revision: 2, revision_of: "c1" }),
    ];
    expect(chainedIssueAttachments(all)).toEqual([]);
    expect(chainedIssueAttachments(undefined)).toEqual([]);
  });
});
