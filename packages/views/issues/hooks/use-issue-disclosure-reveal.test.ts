// @vitest-environment jsdom

import { renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { useIssueDisclosureReveal } from "./use-issue-disclosure-reveal";

describe("useIssueDisclosureReveal", () => {
  it("publishes no token while revealAll is false", () => {
    const { result } = renderHook(() =>
      useIssueDisclosureReveal({
        issueId: "issue-1",
        revealAll: false,
        revealKey: "find:1",
        contentKey: 0,
      }),
    );

    expect(result.current.token).toBeNull();
    expect(result.current.isCommitted("issue-1", "find:1")).toBe(false);
  });

  it("publishes a committed token for the current issue/revealKey once revealAll is true", () => {
    const { result } = renderHook(() =>
      useIssueDisclosureReveal({
        issueId: "issue-1",
        revealAll: true,
        revealKey: "find:1",
        contentKey: 0,
      }),
    );

    expect(result.current.token).not.toBeNull();
    expect(result.current.token?.issueId).toBe("issue-1");
    expect(result.current.token?.revealKey).toBe("find:1");
    expect(result.current.isCommitted("issue-1", "find:1")).toBe(true);
    // A different (stale) revealKey never reads as committed off this token.
    expect(result.current.isCommitted("issue-1", "find:2")).toBe(false);
    expect(result.current.isCommitted("issue-2", "find:1")).toBe(false);
  });

  it("bumps the generation when contentKey changes while still revealing", () => {
    const { result, rerender } = renderHook(
      (props: { contentKey: number }) =>
        useIssueDisclosureReveal({
          issueId: "issue-1",
          revealAll: true,
          revealKey: "find:1",
          contentKey: props.contentKey,
        }),
      { initialProps: { contentKey: 0 } },
    );

    const firstGeneration = result.current.token?.generation;
    expect(firstGeneration).toBe(1);

    rerender({ contentKey: 1 });

    expect(result.current.token?.generation).toBe(2);
    expect(result.current.isCommitted("issue-1", "find:1")).toBe(true);
  });

  it("invalidates the token the moment revealAll flips false — never leaves a stale committed token behind", () => {
    const { result, rerender } = renderHook(
      (props: { revealAll: boolean }) =>
        useIssueDisclosureReveal({
          issueId: "issue-1",
          revealAll: props.revealAll,
          revealKey: "find:1",
          contentKey: 0,
        }),
      { initialProps: { revealAll: true } },
    );

    expect(result.current.token).not.toBeNull();

    rerender({ revealAll: false });

    expect(result.current.token).toBeNull();
    expect(result.current.isCommitted("issue-1", "find:1")).toBe(false);
  });

  it("invalidates the token when revealKey changes (new session/target request) until the new commit lands", () => {
    const { result, rerender } = renderHook(
      (props: { revealKey: string }) =>
        useIssueDisclosureReveal({
          issueId: "issue-1",
          revealAll: true,
          revealKey: props.revealKey,
          contentKey: 0,
        }),
      { initialProps: { revealKey: "target:r2:1" } },
    );

    expect(result.current.isCommitted("issue-1", "target:r2:1")).toBe(true);

    rerender({ revealKey: "target:r2:2" });

    // The new commit for the new key lands synchronously in the same layout
    // effect pass in this test environment, but it must never read as
    // committed under the OLD key once the request has moved on.
    expect(result.current.isCommitted("issue-1", "target:r2:1")).toBe(false);
    expect(result.current.isCommitted("issue-1", "target:r2:2")).toBe(true);
  });

  it("invalidates the token when the issue switches, dropping the old issue's session", () => {
    const { result, rerender } = renderHook(
      (props: { issueId: string }) =>
        useIssueDisclosureReveal({
          issueId: props.issueId,
          revealAll: true,
          revealKey: "find:1",
          contentKey: 0,
        }),
      { initialProps: { issueId: "issue-1" } },
    );

    expect(result.current.isCommitted("issue-1", "find:1")).toBe(true);

    rerender({ issueId: "issue-2" });

    expect(result.current.isCommitted("issue-1", "find:1")).toBe(false);
    expect(result.current.isCommitted("issue-2", "find:1")).toBe(true);
  });
});
