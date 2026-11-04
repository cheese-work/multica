/**
 * @vitest-environment jsdom
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { setApiInstance } from "../api";
import { ApiError, type ApiClient } from "../api/client";
import type { Issue } from "../types";
import { createQueryClient } from "../query-client";
import { issueDetailViewOptions, issueKeys } from "./queries";
import { isIssueUuid, isNotFoundError, useCanonicalIssue } from "./canonical-id";

const ISSUE_UUID = "cb240efb-154c-42a8-ae92-42b02676feca";

const issue = {
  id: ISSUE_UUID,
  workspace_id: "ws-1",
  number: 134,
  identifier: "TRS-134",
  title: "Example",
  status: "todo",
  priority: "medium",
} as Issue;

function createWrapper(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

describe("isIssueUuid", () => {
  it("distinguishes a UUID from an issue identifier", () => {
    expect(isIssueUuid(ISSUE_UUID)).toBe(true);
    expect(isIssueUuid(ISSUE_UUID.toUpperCase())).toBe(true);
    expect(isIssueUuid("TRS-134")).toBe(false);
    expect(isIssueUuid("trs-134")).toBe(false);
    expect(isIssueUuid("")).toBe(false);
  });
});

describe("useCanonicalIssue", () => {
  let qc: QueryClient;
  let getIssue: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    // The app's real client, NOT a bare `new QueryClient()`. Its
    // `staleTime: Infinity` is load-bearing here: under the library default of
    // 0 every cache entry is stale the instant it lands, so a second observer
    // background-refetches and a request-count assertion measures the harness
    // rather than the code.
    qc = createQueryClient();
    getIssue = vi.fn(async () => issue);
    setApiInstance({ getIssue } as unknown as ApiClient);
  });

  afterEach(() => {
    qc.clear();
    vi.restoreAllMocks();
  });

  // A UUID segment needs no resolution step, so the only request is the detail
  // read the issue view needs anyway — never an extra one.
  it("passes a UUID through without a resolution request", async () => {
    const { result } = renderHook(() => useCanonicalIssue("ws-1", ISSUE_UUID), {
      wrapper: createWrapper(qc),
    });

    expect(result.current.canonicalId).toBe(ISSUE_UUID);
    expect(result.current.isResolving).toBe(false);

    await waitFor(() => expect(result.current.issue).toEqual(issue));
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(getIssue).toHaveBeenCalledTimes(1);
    expect(getIssue).toHaveBeenCalledWith(ISSUE_UUID);
  });

  it("resolves an identifier to the issue UUID", async () => {
    const { result } = renderHook(() => useCanonicalIssue("ws-1", "TRS-134"), {
      wrapper: createWrapper(qc),
    });

    expect(result.current.isResolving).toBe(true);
    await waitFor(() => expect(result.current.canonicalId).toBe(ISSUE_UUID));
    expect(result.current.isResolving).toBe(false);
    expect(result.current.issue).toEqual(issue);
    expect(getIssue).toHaveBeenCalledWith("TRS-134");
  });

  // The whole point of `initialData` over a post-resolve cache seed: the
  // canonical query must never observe an empty cache and start its own fetch.
  // A seed from an effect runs after that decision.
  // The common path: an in-app link carries the UUID, the route rewrites the
  // address bar to the identifier, and that rewrite re-renders this hook with
  // the identifier as its segment. Without the identifier-keyed seed the
  // resolution query missed and re-fetched an issue already in hand, so one
  // issue open cost two requests.
  it("costs no extra request when the URL is rewritten to the identifier", async () => {
    const { result, rerender } = renderHook(
      ({ routeId }: { routeId: string }) => useCanonicalIssue("ws-1", routeId),
      { wrapper: createWrapper(qc), initialProps: { routeId: ISSUE_UUID } },
    );

    await waitFor(() => expect(result.current.issue).toEqual(issue));
    expect(getIssue).toHaveBeenCalledTimes(1);

    rerender({ routeId: "TRS-134" });

    await waitFor(() => expect(result.current.canonicalId).toBe(ISSUE_UUID));
    expect(result.current.issue).toEqual(issue);
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(getIssue).toHaveBeenCalledTimes(1);
  });

  it("opens an identifier URL with exactly one request", async () => {
    const { result } = renderHook(() => useCanonicalIssue("ws-1", "TRS-134"), {
      wrapper: createWrapper(qc),
    });

    await waitFor(() => expect(result.current.issue).toEqual(issue));
    // Let any follow-up fetch settle before counting.
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(getIssue).toHaveBeenCalledTimes(1);
  });

  // The detail view mounts once the route resolved the identifier. The row it
  // finds under the UUID came from the detail endpoint a moment ago, so the
  // view's reconcile-on-mount must not read it again.
  it("does not re-read a resolved issue when the detail view mounts on it", async () => {
    const { result } = renderHook(() => useCanonicalIssue("ws-1", "TRS-134"), {
      wrapper: createWrapper(qc),
    });
    await waitFor(() => expect(result.current.issue).toEqual(issue));

    const view = renderHook(
      () => useQuery(issueDetailViewOptions("ws-1", ISSUE_UUID, undefined, result.current.authoritative)),
      { wrapper: createWrapper(qc) },
    );
    await waitFor(() => expect(view.result.current.data).toEqual(issue));
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(getIssue).toHaveBeenCalledTimes(1);
  });

  // An entry patched since the lookup, or one that was already cached, may lack
  // detail-only fields, so it is not authoritative and the view reconciles.
  it("re-reads when the cached entry is not the lookup's response", async () => {
    qc.setQueryData(issueKeys.detail("ws-1", ISSUE_UUID), { ...issue, status: "done" } as Issue);
    const { result } = renderHook(() => useCanonicalIssue("ws-1", "TRS-134"), {
      wrapper: createWrapper(qc),
    });
    await waitFor(() => expect(getIssue).toHaveBeenCalledTimes(1));
    expect(result.current.authoritative).toBe(false);

    renderHook(
      () => useQuery(issueDetailViewOptions("ws-1", ISSUE_UUID, undefined, result.current.authoritative)),
      { wrapper: createWrapper(qc) },
    );
    await waitFor(() => expect(getIssue).toHaveBeenCalledTimes(2));
  });

  // Revisit: an earlier visit left the same object under both keys. Nothing
  // refreshed it on this open, so the view must still reconcile with the
  // detail endpoint, or a missed realtime update stays hidden.
  it("re-reads when both entries are an older visit's leftovers", async () => {
    const older = { updatedAt: Date.now() - 60_000 };
    qc.setQueryData(issueKeys.detail("ws-1", ISSUE_UUID), issue, older);
    qc.setQueryData(issueKeys.detail("ws-1", "TRS-134"), issue, older);
    const { result } = renderHook(() => useCanonicalIssue("ws-1", "TRS-134"), {
      wrapper: createWrapper(qc),
    });
    await waitFor(() => expect(result.current.canonicalId).toBe(ISSUE_UUID));
    expect(result.current.authoritative).toBe(false);

    renderHook(
      () => useQuery(issueDetailViewOptions("ws-1", ISSUE_UUID, undefined, result.current.authoritative)),
      { wrapper: createWrapper(qc) },
    );
    await waitFor(() => expect(getIssue).toHaveBeenCalledTimes(1));
  });

  // A list row or realtime payload omits detail-only fields, so the view must
  // still reconcile with the detail endpoint when it mounts on one.
  it("re-reads when the detail view mounts on a list row", async () => {
    const row = { ...issue, description: "from the list" } as Issue;
    const view = renderHook(() => useQuery(issueDetailViewOptions("ws-1", ISSUE_UUID, row)), {
      wrapper: createWrapper(qc),
    });
    expect(view.result.current.data).toEqual(row);
    await waitFor(() => expect(getIssue).toHaveBeenCalledTimes(1));
  });

  // The realtime updaters patch `issueKeys.detail(wsId, issue.id)` with the
  // UUID from the websocket payload; the resolution response is older and must
  // never overwrite it.
  it("does not clobber a realtime-patched entry", async () => {
    const patched = { ...issue, status: "done" } as Issue;
    qc.setQueryData(issueKeys.detail("ws-1", ISSUE_UUID), patched);

    const { result } = renderHook(() => useCanonicalIssue("ws-1", "TRS-134"), {
      wrapper: createWrapper(qc),
    });

    await waitFor(() => expect(result.current.canonicalId).toBe(ISSUE_UUID));
    expect(result.current.issue).toEqual(patched);
    expect(qc.getQueryData(issueKeys.detail("ws-1", ISSUE_UUID))).toEqual(patched);
  });

  it("reports a nonexistent identifier as terminally not-found", async () => {
    getIssue.mockRejectedValue(new ApiError("issue not found", 404, "Not Found"));

    const { result } = renderHook(() => useCanonicalIssue("ws-1", "ZZZ-134"), {
      wrapper: createWrapper(qc),
    });

    // Generous timeout on purpose: the app's client retries once, so a
    // genuinely unresolvable identifier takes a retry round-trip to settle.
    await waitFor(() => expect(result.current.notFound).toBe(true), { timeout: 5000 });
    expect(result.current.canonicalId).toBeNull();
    expect(result.current.issue).toBeUndefined();
    // Must NOT read as still-resolving: a caller that keeps showing a loading
    // frame here re-enters the remount loop this state exists to prevent.
    expect(result.current.isResolving).toBe(false);
  });

  // Regression: a failed resolution used to present as "not resolving, no id",
  // so the route handed the raw identifier to IssueDetail, whose observer
  // refetched the failed query, flipped this hook back to resolving, unmounted
  // IssueDetail, and remounted it on the next failure — tens of thousands of
  // requests in under a second, with the UI stuck on the skeleton.
  it("settles a failed resolution without looping requests", async () => {
    const noRetry = new QueryClient({
      // Retry off isolates the loop from the app's `retry: 1`, so a count above
      // 1 can only mean a remount refetched.
      defaultOptions: { queries: { staleTime: Infinity, retry: false } },
    });
    getIssue.mockRejectedValue(new ApiError("issue not found", 404, "Not Found"));

    const { result, rerender } = renderHook(() => useCanonicalIssue("ws-1", "ZZZ-134"), {
      wrapper: createWrapper(noRetry),
    });

    await waitFor(() => expect(result.current.notFound).toBe(true));
    const settled = getIssue.mock.calls.length;
    expect(settled).toBe(1);

    rerender();
    await new Promise((resolve) => setTimeout(resolve, 200));
    expect(getIssue).toHaveBeenCalledTimes(settled);
    expect(result.current.notFound).toBe(true);

    noRetry.clear();
  });

  // Regression (CHE-3411): any failed resolution used to read as "deleted".
  // A 401 or a timeout says nothing about whether the issue exists.
  it("reports a non-404 failure as a retryable load error, not not-found", async () => {
    const noRetry = new QueryClient({
      defaultOptions: { queries: { staleTime: Infinity, retry: false } },
    });
    // A 401 is not retried by the lookup policy, so it settles at once.
    getIssue.mockRejectedValueOnce(new ApiError("unauthorized", 401, "Unauthorized"));

    const { result } = renderHook(() => useCanonicalIssue("ws-1", "TRS-134"), {
      wrapper: createWrapper(noRetry),
    });

    await waitFor(() => expect(result.current.loadError).toBe(true));
    expect(result.current.notFound).toBe(false);
    expect(result.current.isResolving).toBe(false);

    result.current.retry();
    await waitFor(() => expect(result.current.canonicalId).toBe(ISSUE_UUID));
    expect(result.current.loadError).toBe(false);

    noRetry.clear();
  });

  // CHE-3535: over a lossy link one dropped request must not reach the error
  // state. The route's lookup backs off and retries transient failures.
  describe("transient lookup failures", () => {
    afterEach(() => {
      vi.useRealTimers();
    });

    it("renders the issue when network failures are followed by success", async () => {
      vi.useFakeTimers();
      getIssue
        .mockRejectedValueOnce(new TypeError("Failed to fetch"))
        .mockRejectedValueOnce(new ApiError("bad gateway", 502, "Bad Gateway"))
        .mockResolvedValue(issue);

      const { result } = renderHook(() => useCanonicalIssue("ws-1", "TRS-134"), {
        wrapper: createWrapper(qc),
      });

      // Still resolving, never failed, while backoff is pending.
      await act(() => vi.advanceTimersByTimeAsync(10));
      expect(result.current.isResolving).toBe(true);
      expect(result.current.loadError).toBe(false);

      await act(() => vi.advanceTimersByTimeAsync(10_000));

      expect(result.current.canonicalId).toBe(ISSUE_UUID);
      expect(result.current.issue).toEqual(issue);
      expect(result.current.loadError).toBe(false);
      expect(result.current.notFound).toBe(false);
      expect(getIssue).toHaveBeenCalledTimes(3);
    });

    it("settles on a load error after bounded retries of a persistent failure", async () => {
      vi.useFakeTimers();
      getIssue.mockRejectedValue(new TypeError("Failed to fetch"));

      const { result } = renderHook(() => useCanonicalIssue("ws-1", "TRS-134"), {
        wrapper: createWrapper(qc),
      });
      await act(() => vi.advanceTimersByTimeAsync(60_000));

      expect(result.current.loadError).toBe(true);
      expect(result.current.notFound).toBe(false);
      expect(getIssue).toHaveBeenCalledTimes(4);
    });

    it("does not retry a 404", async () => {
      vi.useFakeTimers();
      getIssue.mockRejectedValue(new ApiError("not found", 404, "Not Found"));

      const { result } = renderHook(() => useCanonicalIssue("ws-1", "ZZZ-134"), {
        wrapper: createWrapper(qc),
      });
      await act(() => vi.advanceTimersByTimeAsync(60_000));

      expect(result.current.notFound).toBe(true);
      expect(getIssue).toHaveBeenCalledTimes(1);
    });
  });

  it("stays idle without a workspace id", () => {
    const { result } = renderHook(() => useCanonicalIssue("", "TRS-134"), {
      wrapper: createWrapper(qc),
    });

    expect(result.current.canonicalId).toBeNull();
    expect(result.current.isResolving).toBe(false);
    expect(getIssue).not.toHaveBeenCalled();
  });
});

describe("isNotFoundError", () => {
  it("is true only for a 404 ApiError", () => {
    expect(isNotFoundError(new ApiError("gone", 404, "Not Found"))).toBe(true);
    expect(isNotFoundError(new ApiError("denied", 403, "Forbidden"))).toBe(false);
    expect(isNotFoundError(new ApiError("boom", 500, "Internal Server Error"))).toBe(false);
    expect(isNotFoundError(new Error("network down"))).toBe(false);
    expect(isNotFoundError(null)).toBe(false);
  });
});
