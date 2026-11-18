// @vitest-environment jsdom

import { describe, expect, it } from "vitest";
import {
  myIssuesViewStore,
  myIssuesRelationFromScope,
  myIssuesScopeFor,
  type MyIssuesScope,
} from "./my-issues-view-store";

describe("myIssuesRelationFromScope", () => {
  it.each([
    ["all", "all"],
    ["assigned", "assigned"],
    ["created", "created"],
    ["requested", "requested"],
    ["agents", "involved"],
  ] satisfies Array<[MyIssuesScope, string]>)(
    "maps %s to the surface relation %s",
    (scope, relation) => {
      expect(myIssuesRelationFromScope(scope)).toBe(relation);
    },
  );
});

describe("myIssuesViewStore persistence", () => {
  it("persists Requested by me so a reload keeps it selected", () => {
    const { persist } = myIssuesViewStore as unknown as {
      persist: { getOptions(): { partialize?: (s: unknown) => unknown } };
    };
    myIssuesViewStore.getState().setScope("requested");
    const saved = persist.getOptions().partialize?.(myIssuesViewStore.getState());
    expect(saved).toMatchObject({ scope: "requested" });
    expect(myIssuesScopeFor(new URLSearchParams(), "requested")).toBe("requested");
  });
});

describe("myIssuesScopeFor", () => {
  it("opens My agents' work only from the URL", () => {
    expect(myIssuesScopeFor(new URLSearchParams("scope=agents"), "created")).toBe("agents");
    expect(myIssuesScopeFor(new URLSearchParams(), "created")).toBe("created");
  });

  it("reads a stored agents scope as the My issues default", () => {
    expect(myIssuesScopeFor(new URLSearchParams(), "agents")).toBe("assigned");
  });
});
