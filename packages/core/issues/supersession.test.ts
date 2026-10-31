// @vitest-environment node
import { describe, expect, it } from "vitest";
import { supersessionRevisions } from "./supersession";

const link = (id: string) => ({ comment_id: id });

describe("supersessionRevisions", () => {
  it("numbers a comment that supersedes nothing as rev-1", () => {
    expect(supersessionRevisions([{ id: "a" }]).get("a")).toBe(1);
  });

  it("numbers each revision one past the comment it supersedes", () => {
    const revisions = supersessionRevisions([
      { id: "a", superseded_by_id: "b" },
      { id: "b", superseded_by_id: "c", supersedes: [link("a")] },
      { id: "c", supersedes: [link("b")] },
    ]);
    expect([revisions.get("a"), revisions.get("b"), revisions.get("c")]).toEqual([1, 2, 3]);
  });

  it("takes the longest chain when one comment supersedes several", () => {
    const revisions = supersessionRevisions([
      { id: "a", superseded_by_id: "b" },
      { id: "b", superseded_by_id: "d", supersedes: [link("a")] },
      { id: "c", superseded_by_id: "d" },
      { id: "d", supersedes: [link("b"), link("c")] },
    ]);
    expect(revisions.get("d")).toBe(3);
  });

  it("numbers a superseded comment from its link when the superseding comment is not loaded", () => {
    const revisions = supersessionRevisions([{ id: "a", superseded_by_id: "z" }]);
    expect(revisions.get("a")).toBe(1);
    expect(revisions.has("z")).toBe(false);
  });

  it("terminates on a cyclic link", () => {
    const revisions = supersessionRevisions([
      { id: "a", supersedes: [link("b")] },
      { id: "b", supersedes: [link("a")] },
    ]);
    expect(revisions.get("a")).toBeGreaterThan(0);
    expect(revisions.get("b")).toBeGreaterThan(0);
  });
});
