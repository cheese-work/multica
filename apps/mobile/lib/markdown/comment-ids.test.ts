import { describe, expect, it } from "vitest";
import type { CommentPreview } from "@multica/core/types";
import {
  findCommentIds,
  linkifyCommentIds,
  parseCommentMention,
} from "./comment-ids";

const ID = "01a1769e-060e-7a3c-9a0b-123456789abc";
const ISSUE = "01a176f4-37b7-7904-85d6-b7dac9187a61";

const preview = (over: Partial<CommentPreview> = {}): CommentPreview => ({
  comment_id: ID,
  issue_id: ISSUE,
  issue_identifier: "CHE-2120",
  author_type: "agent",
  author_id: "a1",
  author_name: "Luna",
  preview: "Root cause found",
  ...over,
});

const resolved = (p: CommentPreview) => new Map([[p.comment_id, p]]);

describe("findCommentIds", () => {
  it("finds full UUIDs in prose, deduped", () => {
    expect(findCommentIds(`see ${ID} and ${ID}`)).toEqual([ID]);
  });

  it("skips code, links and bare URLs", () => {
    const text = `\`${ID}\` [x](https://a.test/${ID}) https://a.test/c/${ID}`;
    expect(findCommentIds(text)).toEqual([]);
  });

  it("ignores short prefixes", () => {
    expect(findCommentIds("see 01a1769e-060e please")).toEqual([]);
  });
});

describe("linkifyCommentIds", () => {
  it("rewrites a resolved id to a chip link carrying the issue id", () => {
    expect(linkifyCommentIds(`see ${ID}.`, resolved(preview()))).toBe(
      `see [CHE-2120 · Luna: Root cause found](mention://comment/${ID}?issue=${ISSUE}).`,
    );
  });

  it("leaves unresolved ids as plain text", () => {
    expect(linkifyCommentIds(`see ${ID}`, new Map())).toBe(`see ${ID}`);
  });

  it("escapes markdown-breaking characters in the label", () => {
    const out = linkifyCommentIds(ID, resolved(preview({ preview: "a [b] c" })));
    expect(out).toContain("a \\[b\\] c");
  });

  it("omits the author when absent", () => {
    const out = linkifyCommentIds(ID, resolved(preview({ author_name: "" })));
    expect(out.startsWith("[CHE-2120 · Root cause found]")).toBe(true);
  });

  it("does not rewrite inside code or links", () => {
    const text = `\`${ID}\``;
    expect(linkifyCommentIds(text, resolved(preview()))).toBe(text);
  });
});

describe("parseCommentMention", () => {
  it("parses comment and issue ids", () => {
    expect(
      parseCommentMention(`mention://comment/${ID}?issue=${ISSUE}`),
    ).toEqual({ commentId: ID, issueId: ISSUE });
  });

  it("rejects non-comment and malformed urls", () => {
    expect(parseCommentMention(`mention://issue/${ID}`)).toBeNull();
    expect(parseCommentMention(`mention://comment/${ID}`)).toBeNull();
    expect(parseCommentMention(`mention://comment/not-a-uuid?issue=${ISSUE}`)).toBeNull();
  });
});
