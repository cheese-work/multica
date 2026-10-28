// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { Attachment } from "../types/attachment";
import { defaultPlanBase, diffRevisionLines, earlierPlanRevisions, planRevisionNumber, revisionSegments } from "./plan-revisions";

const attachment = (id: string, filename: string, extra: Partial<Attachment> = {}): Attachment => ({ id, filename, workspace_id: "workspace", issue_id: "issue", comment_id: null, chat_session_id: null, chat_message_id: null, uploader_id: "author", uploader_type: "agent", url: "", download_url: "", markdown_url: "", content_type: "text/markdown", size_bytes: 10, created_at: "2026-10-27T01:00:00Z", ...extra });

describe("same-issue plan revision lookup", () => {
  it("matches only the exact positive safe-integer filename convention", () => {
    expect(planRevisionNumber("plan-rev-12.md")).toBe(12);
    for (const filename of ["plan-rev-0.md", "plan-rev-01.md", "plan-rev-9007199254740992.md", "PLAN-rev-1.md", "plan-rev-1.md.bak", "plan-rev--1.md", "notes.md"]) expect(planRevisionNumber(filename)).toBeNull();
  });
  it("orders numeric gaps and excludes other issues, workspaces, unrelated and later files", () => {
    const current = attachment("current", "plan-rev-12.md");
    const bases = earlierPlanRevisions(current, [attachment("one", "plan-rev-1.md"), attachment("ten", "plan-rev-10.md"), attachment("other-issue", "plan-rev-11.md", { issue_id: "another" }), attachment("foreign", "plan-rev-11.md", { workspace_id: "another" }), current, attachment("later", "plan-rev-13.md"), attachment("notes", "notes.md")]);
    expect(bases.map((base) => base.revision)).toEqual([10, 1]);
    expect(defaultPlanBase(bases)).toBe("ten");
    expect(earlierPlanRevisions({ ...current, issue_id: null }, bases.map((base) => base.attachment))).toEqual([]);
  });
  it("does not guess duplicate bases from upload times and never falls back to an older group", () => {
    const bases = earlierPlanRevisions(attachment("current", "plan-rev-3.md"), [attachment("one", "plan-rev-1.md"), attachment("two-a", "plan-rev-2.md"), attachment("two-b", "plan-rev-2.md", { created_at: "2026-10-27T03:00:00Z" })]);
    expect(bases).toHaveLength(3);
    expect(defaultPlanBase(bases)).toBeNull();
    expect(earlierPlanRevisions(attachment("first", "plan-rev-1.md"), [])).toEqual([]);
  });
});

describe("literal plan line diff", () => {
  it("reconstructs both versions, including blank lines and non-executable markup", () => {
    const before = "# Plan\nkeep\nold\n\n<script>alert(1)</script>\n";
    const after = "# Plan\nkeep\nnew\nextra\n\n<script>alert(1)</script>\n";
    const lines = diffRevisionLines(before, after);
    expect(lines.filter((line) => line.kind !== "add").map((line) => line.text).join("\n")).toBe(before);
    expect(lines.filter((line) => line.kind !== "remove").map((line) => line.text).join("\n")).toBe(after);
    expect(lines.filter((line) => line.kind === "remove").map((line) => line.oldNumber)).toEqual([3]);
    expect(lines.filter((line) => line.kind === "add").map((line) => line.newNumber)).toEqual([3, 4]);
  });
  it("handles empty, identical, CRLF and a hundreds-of-lines fixture", () => {
    expect(diffRevisionLines("", "")).toEqual([]);
    expect(diffRevisionLines("same", "same")).toEqual([{ kind: "same", text: "same", oldNumber: 1, newNumber: 1 }]);
    expect(diffRevisionLines("old", "")).toEqual([{ kind: "remove", text: "old", oldNumber: 1 }]);
    expect(diffRevisionLines("", "new")).toEqual([{ kind: "add", text: "new", newNumber: 1 }]);
    expect(diffRevisionLines("one\r\ntwo", "one\ntwo").every((line) => line.kind === "same")).toBe(true);
    const base = Array.from({ length: 340 }, (_, index) => `Line ${index + 1}`);
    const current = base.slice();
    current[105] = "Replacement";
    current.splice(220, 0, "Added");
    const lines = diffRevisionLines(base.join("\n"), current.join("\n"));
    expect(lines.filter((line) => line.kind === "add")).toHaveLength(2);
    expect(lines.filter((line) => line.kind === "remove")).toHaveLength(1);
    const segments = revisionSegments(lines);
    expect(segments.filter((segment) => segment.kind === "change")).toHaveLength(2);
    expect(segments.some((segment) => segment.kind === "context" && segment.lines.length > 100)).toBe(true);
    expect(segments.flatMap((segment) => segment.lines)).toEqual(lines);
  });
  it("bounds expensive comparisons rather than freezing on oversized inputs", () => {
    expect(() => diffRevisionLines("old\n".repeat(3000), "new\n".repeat(3000))).toThrow("comparison_too_large");
    expect(() => diffRevisionLines("\n".repeat(12_000), "\n".repeat(12_000))).toThrow("comparison_too_large");
  });
});
