"use client";

import { useEffect, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useCanonicalIssue } from "@multica/core/issues/canonical-id";
import { useWorkspaceId } from "@multica/core/hooks";
import { useMarkMentionsReadOnOpen } from "@multica/core/inbox";
import { issueTasksOptions } from "@multica/core/issues/queries";
import { useActorName } from "@multica/core/workspace/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { useNavigation } from "../../navigation";
import { DecisionActionPanel } from "./decision-action-panel";
import { TranscriptButton } from "../../common/task-transcript";
import { IssueDetail, IssueDetailSkeleton, IssueNotFound } from "./issue-detail";
import { IssuePeekHost } from "./issue-peek";

interface IssueDetailRouteProps {
  /**
   * Raw `/{ws}/issues/{segment}` parameter. Either a UUID (older links, and
   * anything the app itself linked before the issue row was in hand) or a
   * human-readable identifier such as `MUL-123`.
   */
  routeId: string;
  onDelete?: () => void;
}

/**
 * Rewrite `/{ws}/issues/{uuid}` to `/{ws}/issues/{identifier}` once the issue
 * is known, so the address bar and any copied URL read as `MUL-123`.
 *
 * A replace, not a push: the UUID URL is the same page, and a history entry
 * for it would make Back bounce the user between two spellings of one issue.
 */
export function useCanonicalIssueUrl(
  routeId: string,
  identifier: string | undefined,
  hash = "",
  search = "",
) {
  const paths = useWorkspacePaths();
  const navigation = useNavigation();
  const canonicalHref = identifier ? `${paths.issueDetail(identifier)}${search}${hash}` : null;
  // `useWorkspacePaths()` and the navigation adapter are both rebuilt on
  // render, so this ref — not the dependency array — is what guarantees the
  // replace runs once per target instead of on every commit.
  const replacedRef = useRef<string | null>(null);

  useEffect(() => {
    if (!canonicalHref || routeId === identifier) return;
    if (replacedRef.current === canonicalHref) return;
    replacedRef.current = canonicalHref;
    navigation.replace(canonicalHref);
  }, [canonicalHref, identifier, routeId, navigation]);
}

export function parseCommentHighlightHash(hash: string): string | undefined {
  const match = /^#comment-([A-Za-z0-9_-]+)$/.exec(hash);
  return match?.[1];
}

function useCommentHighlightHash(): { hash: string; commentId?: string } {
  const { hash } = useNavigation();

  return { hash, commentId: parseCommentHighlightHash(hash) };
}

/**
 * `?run=<task id>` opens that run's transcript, the existing run view, over the
 * issue: a decision card links the run that picked its answer up. A run that is
 * not on this issue opens nothing. Closing drops the parameter from the address.
 */
function RequestedRun({ issueId, runId, path }: { issueId: string; runId: string; path: string }) {
  const navigation = useNavigation();
  const { getActorName } = useActorName();
  const { data: tasks = [] } = useQuery(issueTasksOptions(issueId));
  const [closed, setClosed] = useState(false);
  const task = tasks.find((candidate) => candidate.id === runId);
  if (!task) return null;
  return (
    <TranscriptButton
      task={task}
      agentName={getActorName("agent", task.agent_id)}
      isLive={["queued", "dispatched", "running"].includes(task.status)}
      renderButton={false}
      open={!closed}
      onOpenChange={(open) => {
        if (open) return;
        setClosed(true);
        navigation.replace(path);
      }}
    />
  );
}

/**
 * Route-level wrapper around `IssueDetail` for `/{ws}/issues/{id}`.
 *
 * Two jobs the panel-embedded `IssueDetail` must not do:
 *  - resolve an identifier segment to the issue's UUID before rendering, so
 *    every cache key below stays UUID-keyed and realtime updates land (see
 *    `useCanonicalIssue`);
 *  - rewrite the address bar to the canonical identifier URL. That belongs to
 *    the route and only the route — the inbox renders `IssueDetail` in a side
 *    panel, where replacing the URL would navigate the user out of the inbox.
 */
export function IssueDetailRoute({ routeId, onDelete }: IssueDetailRouteProps) {
  const wsId = useWorkspaceId();
  const { canonicalId, issue, isResolving, notFound, loadError, dataError, retry, authoritative } = useCanonicalIssue(wsId, routeId);
  const highlight = useCommentHighlightHash();
  const { searchParams } = useNavigation();
  const paths = useWorkspacePaths();
  const runId = searchParams.get("run");
  // The approve URL a run-budget request prints names the action to approve.
  const approveId = searchParams.get("approve");
  const keep = new URLSearchParams();
  if (runId) keep.set("run", runId);
  if (approveId) keep.set("approve", approveId);

  useCanonicalIssueUrl(routeId, issue?.identifier, highlight.hash, keep.size ? `?${keep.toString()}` : "");
  useMarkMentionsReadOnOpen(canonicalId);

  if (isResolving) return <IssueDetailSkeleton />;

  // Render not-found here rather than handing the unresolved segment down.
  // `IssueDetail` would mount a second observer on the query that just failed,
  // refetch it, and restart this component's resolve/remount cycle — an
  // unbounded request loop that never settles. See `CanonicalIssue.notFound`.
  if (loadError) return <IssueNotFound showBackLink={!onDelete} onRetry={retry} dataError={dataError} issueKey={routeId} />;
  if (notFound || !canonicalId) return <IssueNotFound showBackLink={!onDelete} />;

  // The page hosts a side peek so Shift+Click on an issue mention in the
  // description or comments opens it floating over the page. Keyed so moving
  // to another issue closes the peek.
  return (
    <IssuePeekHost key={canonicalId}>
      <IssueDetail
        issueId={canonicalId}
        onDelete={onDelete}
        highlightCommentId={highlight.commentId}
        authoritative={authoritative}
      />
      {approveId && <DecisionActionPanel key={approveId} actionId={approveId} issueId={canonicalId} issueLabel={issue?.identifier ?? routeId} />}
      {runId && <RequestedRun key={runId} issueId={canonicalId} runId={runId} path={`${paths.issueDetail(issue?.identifier ?? routeId)}${highlight.hash}`} />}
    </IssuePeekHost>
  );
}
