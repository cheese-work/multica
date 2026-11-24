"use client";

<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
import { useEffect, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
||||||| 2ae2f08005
import { useEffect, useRef } from "react";
=======
import { useCallback, useEffect, useRef } from "react";
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
import { useCanonicalIssue } from "@multica/core/issues/canonical-id";
import { useWorkspaceId } from "@multica/core/hooks";
import { useMarkMentionsReadOnOpen } from "@multica/core/inbox";
import { issueTasksOptions } from "@multica/core/issues/queries";
import { useActorName } from "@multica/core/workspace/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { useInboxSelectionStore } from "@multica/core/inbox/selection-store";
import { useNavigation } from "../../navigation";
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
import { DecisionActionPanel } from "./decision-action-panel";
import { TranscriptButton } from "../../common/task-transcript";
import { IssueDetail, IssueDetailSkeleton, IssueNotFound } from "./issue-detail";
||||||| 2ae2f08005
import { IssueDetail, IssueDetailSkeleton, IssueNotFound } from "./issue-detail";
=======
import { IssueDetail, IssueDetailSkeleton, IssueNotFound, IssueLoadError } from "./issue-detail";
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
import { IssuePeekHost } from "./issue-peek";

interface IssueDetailRouteProps {
  /**
   * Raw `/{ws}/issues/{segment}` parameter. Either a UUID (older links, and
   * anything the app itself linked before the issue row was in hand) or a
   * human-readable identifier such as `MUL-123`.
   */
  routeId: string;
  onDelete?: () => void;
  /**
   * The decoded file name of a `/{ws}/issues/{segment}/deliverables/{name}`
   * link: open the newest version of that deliverable.
   */
  deliverable?: string;
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
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
  search = "",
||||||| 2ae2f08005
=======
  deliverable?: string,
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
) {
  const paths = useWorkspacePaths();
  const navigation = useNavigation();
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
  const canonicalHref = identifier ? `${paths.issueDetail(identifier)}${search}${hash}` : null;
||||||| 2ae2f08005
  const canonicalHref = identifier ? `${paths.issueDetail(identifier)}${hash}` : null;
=======
  // A deliverable link keeps its `/deliverables/{name}` tail: dropping it
  // would turn the address into a plain issue link while the file is open.
  const canonicalHref = identifier
    ? `${deliverable ? paths.issueDeliverable(identifier, deliverable) : paths.issueDetail(identifier)}${hash}`
    : null;
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
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

/** `#app-request-<id>`: an app's confirmation request on the issue, as a notification links it. */
export function parseAppRequestHighlightHash(hash: string): string | undefined {
  const match = /^#app-request-([A-Za-z0-9_-]+)$/.exec(hash);
  return match?.[1];
}

function useHighlightHash(): { hash: string; commentId?: string; appRequestId?: string } {
  const { hash } = useNavigation();

  return { hash, commentId: parseCommentHighlightHash(hash), appRequestId: parseAppRequestHighlightHash(hash) };
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
export function IssueDetailRoute({ routeId, onDelete, deliverable }: IssueDetailRouteProps) {
  const wsId = useWorkspaceId();
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
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
||||||| 2ae2f08005
  const { canonicalId, issue, isResolving, notFound } = useCanonicalIssue(wsId, routeId);
  const highlight = useCommentHighlightHash();
=======
  const {
    canonicalId, issue, isResolving, notFound, loadFailed, isRetrying, retry, hasLoaded,
  } = useCanonicalIssue(wsId, routeId);
  const highlight = useHighlightHash();
  const landingToken = useInboxSelectionStore((s) =>
    s.landing && s.landing.issueId === canonicalId ? s.landing.token : 0,
  );
  const paths = useWorkspacePaths();
  const navigation = useNavigation();
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d

<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
  useCanonicalIssueUrl(routeId, issue?.identifier, highlight.hash, keep.size ? `?${keep.toString()}` : "");
  useMarkMentionsReadOnOpen(canonicalId);
||||||| 2ae2f08005
  useCanonicalIssueUrl(routeId, issue?.identifier, highlight.hash);
=======
  useCanonicalIssueUrl(routeId, issue?.identifier, highlight.hash, deliverable);
  // A UUID or differently-cased address is about to be rewritten, which mounts
  // a new page (a new route segment on web). Landing here would scroll a page
  // that is about to go, and record the landing as done for the next one.
  const landsHere = routeId === issue?.identifier;
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d

  // Once the link has opened its file and the viewer has closed (or the file
  // is missing), the address becomes the plain issue page. A replace: Back
  // should leave the issue, not re-open the file.
  const issueRef = issue?.identifier ?? routeId;
  const latestNav = useRef({ paths, navigation });
  latestNav.current = { paths, navigation };
  const onDeliverableLinkDone = useCallback(() => {
    const { paths, navigation } = latestNav.current;
    navigation.replace(paths.issueDetail(issueRef));
  }, [issueRef]);

  if (loadFailed) {
    return <IssueLoadError showBackLink={!onDelete} onRetry={retry} isRetrying={isRetrying} />;
  }

  if (isResolving || (!issue && !notFound)) return <IssueDetailSkeleton />;

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
        skipInitialRefetch={hasLoaded}
        onDelete={onDelete}
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
        highlightCommentId={highlight.commentId}
        authoritative={authoritative}
||||||| 2ae2f08005
        highlightCommentId={highlight.commentId}
=======
        highlightCommentId={landsHere ? highlight.commentId : undefined}
        highlightAppRequestId={landsHere ? highlight.appRequestId : undefined}
        highlightRequestToken={landingToken}
        openDeliverable={deliverable}
        onDeliverableLinkDone={deliverable ? onDeliverableLinkDone : undefined}
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
      />
      {approveId && <DecisionActionPanel key={approveId} actionId={approveId} issueId={canonicalId} issueLabel={issue?.identifier ?? routeId} />}
      {runId && <RequestedRun key={runId} issueId={canonicalId} runId={runId} path={`${paths.issueDetail(issue?.identifier ?? routeId)}${highlight.hash}`} />}
    </IssuePeekHost>
  );
}
