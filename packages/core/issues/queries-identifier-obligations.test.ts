// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryObserver } from "@tanstack/react-query";

import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import type { Issue } from "../types";
import { issueDetailOptions, issueIdentifierOptions, issueKeys } from "./queries";
import { onIssueAuxiliaryRevision } from "./ws-updaters";

// CHE-1829 G19-F1. A bare-key lookup seeds the detail cache with
// `setQueryData`, which clears the stale flag. With the global
// `staleTime: Infinity` the seeded entry then stays fresh unless it owes a
// refetch. An unversioned lookup response is admitted when no cached projection
// has a revision, so it must still owe a refetch when an auxiliary event already
// proved a newer revision exists.

const WS_ID = "ws-1";
const detailKey = issueKeys.detail(WS_ID, "issue-7");

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

const flushTimers = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

function installFakeIssueApi(getIssue: (id: string) => Promise<Issue>) {
  setApiInstance({ getIssue } as unknown as ApiClient);
}

describe("identifier lookup seeding (CHE-1829 G19-F1)", () => {
  let qc: QueryClient;

  const lookup = (response: Issue) => {
    installFakeIssueApi(vi.fn().mockResolvedValue(response));
    return qc.fetchQuery(issueIdentifierOptions(WS_ID, "MUL-7"));
  };

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  });

  afterEach(() => qc.clear());

  it("leaves seeded detail owing a refetch when an unversioned response meets a recorded auxiliary revision", async () => {
    onIssueAuxiliaryRevision(qc, WS_ID, "issue-7", 6);

    await lookup(makeIssue({ title: "unversioned lookup", revision: undefined }));
    await flushTimers();

    expect(qc.getQueryData<Issue>(detailKey)).toMatchObject({ title: "unversioned lookup" });
    expect(qc.getQueryState(detailKey)?.isInvalidated).toBe(true);
  });

  it("makes the invalidated detail refetch when it is next observed", async () => {
    onIssueAuxiliaryRevision(qc, WS_ID, "issue-7", 6);
    await lookup(makeIssue({ title: "unversioned lookup", revision: undefined }));
    const getIssue = vi
      .fn<(id: string) => Promise<Issue>>()
      .mockResolvedValue(makeIssue({ title: "authoritative", revision: 6 }));
    installFakeIssueApi(getIssue);

    const observer = new QueryObserver(qc, { ...issueDetailOptions(WS_ID, "issue-7"), staleTime: Infinity });
    const stop = observer.subscribe(() => {});
    await vi.waitFor(() => expect(observer.getCurrentResult().data).toMatchObject({ revision: 6 }));
    stop();

    expect(getIssue).toHaveBeenCalledTimes(1);
  });

  it("does not invalidate seeded detail for an auxiliary revision of another issue", async () => {
    onIssueAuxiliaryRevision(qc, WS_ID, "issue-8", 6);

    await lookup(makeIssue({ revision: undefined }));
    await flushTimers();

    expect(qc.getQueryState(detailKey)?.isInvalidated).toBe(false);
  });

  it("does not invalidate seeded detail for an auxiliary revision recorded in another workspace", async () => {
    onIssueAuxiliaryRevision(qc, "ws-2", "issue-7", 6);

    await lookup(makeIssue({ revision: undefined }));
    await flushTimers();

    expect(qc.getQueryState(detailKey)?.isInvalidated).toBe(false);
  });

  it("does not invalidate an unversioned seed when no auxiliary revision was recorded", async () => {
    await lookup(makeIssue({ revision: undefined }));
    await flushTimers();

    expect(qc.getQueryState(detailKey)?.isInvalidated).toBe(false);
  });

  it("does not invalidate a versioned seed that has caught up with the auxiliary revision", async () => {
    onIssueAuxiliaryRevision(qc, WS_ID, "issue-7", 6);

    await lookup(makeIssue({ revision: 6 }));
    await flushTimers();

    expect(qc.getQueryState(detailKey)?.isInvalidated).toBe(false);
  });
});
