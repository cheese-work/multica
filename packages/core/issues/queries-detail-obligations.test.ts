// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient } from "@tanstack/react-query";

import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import type { Issue } from "../types";
import { issueDetailOptions, issueKeys } from "./queries";
import { onIssueAuxiliaryRevision } from "./ws-updaters";

// CHE-1829 OCR-F1. With the global `staleTime: Infinity`, a detail entry that is
// behind another projection stays fresh forever unless it is flagged
// `isInvalidated`. These cases lose that flag without any API response arriving
// late: the fetch is cancelled and its revert erases the flag, or the cached
// detail carries no revision to compare.

const WS_ID = "ws-1";
const detailKey = issueKeys.detail(WS_ID, "issue-7");
const listKey = issueKeys.list(WS_ID);

function makeIssue(overrides: Partial<Issue> = {}): Issue {
  return {
    id: "issue-7",
    workspace_id: WS_ID,
    number: 7,
    identifier: "MUL-7",
    title: "Issue 7",
    description: null,
    status: "todo",
    priority: "none",
    assignee_type: null,
    assignee_id: null,
    creator_type: "member",
    creator_id: "user-1",
    parent_issue_id: null,
    project_id: null,
    position: 7,
    stage: null,
    start_date: null,
    due_date: null,
    labels: [],
    metadata: {},
    properties: {},
    created_at: "2025-01-01T00:00:00Z",
    updated_at: "2025-01-01T00:00:00Z",
    ...overrides,
  };
}

const seedList = (issue: Issue) => ({
  byStatus: { unstarted: { issues: [issue], total: 1 } },
});

const flushTimers = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

function installFakeIssueApi(getIssue: (id: string) => Promise<Issue>) {
  setApiInstance({ getIssue } as unknown as ApiClient);
}

describe("uuid detail obligations (CHE-1829 OCR-F1)", () => {
  let qc: QueryClient;

  const detailFetch = () => ({
    ...issueDetailOptions(WS_ID, "issue-7"),
    staleTime: 0,
    retry: false,
  });

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  });

  afterEach(() => qc.clear());

  it("keeps the refetch obligation when the fetch is cancelled and the API never responds", async () => {
    qc.setQueryData(detailKey, makeIssue({ title: "cached", revision: 5 }));
    const getIssue = vi.fn<(id: string) => Promise<Issue>>().mockReturnValue(new Promise(() => {}));
    installFakeIssueApi(getIssue);
    const pending = qc.fetchQuery(detailFetch()).catch(() => undefined);
    await vi.waitFor(() => expect(getIssue).toHaveBeenCalledTimes(1));
    onIssueAuxiliaryRevision(qc, WS_ID, "issue-7", 6);

    await qc.cancelQueries({ queryKey: detailKey, exact: true });
    await pending;
    await flushTimers();

    expect(qc.getQueryData<Issue>(detailKey)).toMatchObject({ revision: 5, title: "cached" });
    expect(qc.getQueryState(detailKey)?.isInvalidated).toBe(true);
  });

  it("keeps unversioned detail owing a refetch when a response is rejected beside a versioned projection", async () => {
    qc.setQueryData(detailKey, makeIssue({ title: "unversioned detail", revision: undefined }));
    qc.setQueryData(listKey, seedList(makeIssue({ revision: 7 })));
    installFakeIssueApi(vi.fn().mockResolvedValue(makeIssue({ title: "behind list", revision: 6 })));

    const result = await qc.fetchQuery(detailFetch());
    await flushTimers();

    expect(result).toMatchObject({ title: "unversioned detail" });
    expect(qc.getQueryState(detailKey)?.isInvalidated).toBe(true);
  });

  it("keeps unversioned detail owing a refetch when an auxiliary revision is pending", async () => {
    qc.setQueryData(detailKey, makeIssue({ title: "unversioned detail", revision: undefined }));
    let resolveResponse!: (issue: Issue) => void;
    installFakeIssueApi(
      vi.fn().mockImplementation(() => new Promise((resolve) => (resolveResponse = resolve))),
    );
    const pending = qc.fetchQuery(detailFetch());
    await vi.waitFor(() => expect(resolveResponse).toBeDefined());
    onIssueAuxiliaryRevision(qc, WS_ID, "issue-7", 6);

    resolveResponse(makeIssue({ title: "unversioned response", revision: undefined }));
    await pending;
    await flushTimers();

    expect(qc.getQueryState(detailKey)?.isInvalidated).toBe(true);
  });

  it("does not invalidate detail that is unversioned beside unversioned projections", async () => {
    qc.setQueryData(detailKey, makeIssue({ revision: undefined }));
    qc.setQueryData(listKey, seedList(makeIssue({ revision: undefined })));
    installFakeIssueApi(vi.fn().mockResolvedValue(makeIssue({ title: "fresh", revision: undefined })));

    await qc.fetchQuery(detailFetch());
    await flushTimers();

    expect(qc.getQueryState(detailKey)?.isInvalidated).toBe(false);
  });

  it("leaves no cache listener behind after a cancelled fetch that never responds", async () => {
    const cache = qc.getQueryCache();
    const subscribe = cache.subscribe.bind(cache);
    let opened = 0;
    let released = 0;
    vi.spyOn(cache, "subscribe").mockImplementation((listener) => {
      opened += 1;
      const release = subscribe(listener);
      return () => {
        released += 1;
        release();
      };
    });
    const getIssue = vi.fn<(id: string) => Promise<Issue>>().mockReturnValue(new Promise(() => {}));
    installFakeIssueApi(getIssue);
    const pending = qc.fetchQuery(detailFetch()).catch(() => undefined);
    await vi.waitFor(() => expect(getIssue).toHaveBeenCalledTimes(1));

    await qc.cancelQueries({ queryKey: detailKey, exact: true });
    await pending;
    await flushTimers();

    expect(opened - released).toBe(0);
  });
});
