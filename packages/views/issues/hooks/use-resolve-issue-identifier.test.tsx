import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "@multica/core/api";
import { createQueryClient } from "@multica/core/query-client";
import type { Issue } from "@multica/core/types";
import { useResolveIssueIdentifier } from "./use-resolve-issue-identifier";

vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: { getIssue: vi.fn() },
}));

const workspace = vi.hoisted(() => ({
  current: { id: "workspace-1", issue_prefix: "MUL" },
}));

vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/paths")>()),
  useCurrentWorkspace: () => workspace.current,
}));

function makeIssue(number: number, workspaceId = "workspace-1"): Issue {
  return {
    id: `${number}${number}${number}`,
    workspace_id: workspaceId,
    number,
    identifier: `MUL-${number}`,
  } as unknown as Issue;
}

describe("useResolveIssueIdentifier", () => {
  let queryClient: ReturnType<typeof createQueryClient>;
  let release: Array<() => void>;

  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );

  // Holds each lookup open until the test releases it, so the loading state is
  // observable.
  function delayedLookup(
    resolve: (identifier: string) => Issue | ApiError | undefined,
  ) {
    vi.mocked(api.getIssue).mockImplementation(
      (identifier) =>
        new Promise((done, fail) => {
          release.push(() => {
            const outcome = resolve(identifier);
            if (outcome instanceof Error) fail(outcome);
            else done(outcome as Issue);
          });
        }),
    );
  }

  beforeEach(() => {
    queryClient = createQueryClient();
    queryClient.setDefaultOptions({
      queries: { ...queryClient.getDefaultOptions().queries, retry: false },
    });
    release = [];
    workspace.current = { id: "workspace-1", issue_prefix: "MUL" };
    vi.mocked(api.getIssue).mockReset();
  });

  afterEach(() => {
    cleanup();
    queryClient.clear();
  });

  it("reports loading, not a miss, while the lookup is in flight", async () => {
    delayedLookup((id) => makeIssue(Number(id.slice(4))));
    const { result } = renderHook(() => useResolveIssueIdentifier("MUL-7"), { wrapper });

    await waitFor(() => expect(api.getIssue).toHaveBeenCalledTimes(1));
    expect(result.current).toBeUndefined();

    await act(async () => release[0]!());
    await waitFor(() => expect(result.current?.identifier).toBe("MUL-7"));
  });

  it("settles a 404 to null, which callers render as plain text", async () => {
    delayedLookup(() => new ApiError("Not found", 404, "Not Found"));
    const { result } = renderHook(() => useResolveIssueIdentifier("MUL-9"), { wrapper });

    await waitFor(() => expect(api.getIssue).toHaveBeenCalledTimes(1));
    expect(result.current).toBeUndefined();

    await act(async () => release[0]!());
    await waitFor(() => expect(result.current).toBeNull());
  });

  it("settles a non-404 failure to null only after the query gives up", async () => {
    delayedLookup(() => new ApiError("Unavailable", 503, "Unavailable"));
    const { result } = renderHook(() => useResolveIssueIdentifier("MUL-9"), { wrapper });

    await waitFor(() => expect(api.getIssue).toHaveBeenCalledTimes(1));
    expect(result.current).toBeUndefined();

    await act(async () => release[0]!());
    await waitFor(() => expect(result.current).toBeNull());
    expect(queryClient.getQueryState(["issues", "workspace-1", "identifier", "MUL-9"])?.status)
      .toBe("error");
  });

  it("never shows the previous identifier's issue while the next one loads", async () => {
    delayedLookup((id) => makeIssue(Number(id.slice(4))));
    const { result, rerender } = renderHook(
      ({ identifier }) => useResolveIssueIdentifier(identifier),
      { wrapper, initialProps: { identifier: "MUL-7" } },
    );
    await waitFor(() => expect(release).toHaveLength(1));
    await act(async () => release[0]!());
    await waitFor(() => expect(result.current?.identifier).toBe("MUL-7"));

    rerender({ identifier: "MUL-8" });

    await waitFor(() => expect(release).toHaveLength(2));
    expect(result.current).toBeUndefined();
    await act(async () => release[1]!());
    await waitFor(() => expect(result.current?.identifier).toBe("MUL-8"));
  });

  it("keeps the issue on screen while an alias of the same issue loads", async () => {
    // The server reads `MUL-007` as number 7, so it is the same issue as `MUL-7`
    // under a different query key.
    delayedLookup((id) => makeIssue(Number(id.slice(4))));
    const { result, rerender } = renderHook(
      ({ identifier }) => useResolveIssueIdentifier(identifier),
      { wrapper, initialProps: { identifier: "MUL-7" } },
    );
    await waitFor(() => expect(release).toHaveLength(1));
    await act(async () => release[0]!());
    await waitFor(() => expect(result.current?.identifier).toBe("MUL-7"));

    rerender({ identifier: "MUL-007" });

    await waitFor(() => expect(release).toHaveLength(2));
    expect(result.current?.identifier).toBe("MUL-7");
    await act(async () => release[1]!());
    await waitFor(() => expect(queryClient.getQueryState(["issues", "workspace-1", "identifier", "MUL-007"])?.status).toBe("success"));
    expect(result.current?.identifier).toBe("MUL-7");
  });

  it("never shows another workspace's issue after a workspace switch", async () => {
    delayedLookup((id) => makeIssue(Number(id.slice(4)), workspace.current.id));
    const { result, rerender } = renderHook(() => useResolveIssueIdentifier("MUL-7"), {
      wrapper,
    });
    await waitFor(() => expect(release).toHaveLength(1));
    await act(async () => release[0]!());
    await waitFor(() => expect(result.current?.workspace_id).toBe("workspace-1"));

    workspace.current = { id: "workspace-2", issue_prefix: "MUL" };
    rerender();

    await waitFor(() => expect(release).toHaveLength(2));
    expect(result.current).toBeUndefined();
    await act(async () => release[1]!());
    await waitFor(() => expect(result.current?.workspace_id).toBe("workspace-2"));
  });
});
