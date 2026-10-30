// @vitest-environment node
import { describe, expect, it } from "vitest";
import { mediaPath, parseMediaPath } from "./media";

describe("media routes", () => {
  it("round trips source-scoped stable routes", () => {
    const route = { workspaceSlug: "acme", type: "issues" as const, id: "issue-1", kind: "mermaid" as const, mediaId: "hash-1" };
    expect(parseMediaPath(mediaPath(route))).toEqual(route);
  });
  it.each(["https://example.test/acme/media/issues/id/html/key", "//example.test/acme/media/issues/id/html/key", "/acme/media/issues/../html/key", "/acme/media/issues/%2f/html/key", "/acme/media/issues/id/video/key", "/acme/media/issues/id/html/key?url=secret", "/acme/media/issues/id/html/key/extra"])("rejects unsafe or unsupported paths %s", (path) => {
    expect(parseMediaPath(path)).toBeNull();
  });
});
