import { describe, expect, it, vi } from "vitest";

import { validatedWorkspaces } from "./workspaces";

vi.mock("@/data/api", () => ({ api: {} }));

const ws = [{ id: "ws1" }];
const settled = { isSuccess: true, isFetching: false };

describe("validatedWorkspaces", () => {
  it("gives the list of a settled, validated response", () => {
    expect(
      validatedWorkspaces({ ...settled, data: { workspaces: ws, validated: true } }),
    ).toEqual(ws);
  });

  it("gives a validated empty list: the last workspace was deleted", () => {
    expect(
      validatedWorkspaces({ ...settled, data: { workspaces: [], validated: true } }),
    ).toEqual([]);
  });

  it.each([
    ["a fallback response", { ...settled, data: { workspaces: [], validated: false } }],
    ["a failed fetch", { isSuccess: false, isFetching: false, data: undefined }],
    ["a refetch in flight", { ...settled, isFetching: true, data: { workspaces: ws, validated: true } }],
  ])("gives nothing for %s", (_name, query) => {
    expect(validatedWorkspaces(query)).toBeNull();
  });
});
