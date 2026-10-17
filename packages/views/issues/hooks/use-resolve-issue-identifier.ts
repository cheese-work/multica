"use client";

import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { issueIdentifierOptions } from "@multica/core/issues/queries";
import { useCurrentWorkspace } from "@multica/core/paths";
import { isIssueIdentifier } from "@multica/ui/markdown";
import type { Issue } from "@multica/core/types";

const IDENTIFIER_PARTS = /^([A-Z][A-Z0-9]*)-(\d+)$/;

/** True when `identifier` names `issue`. The server reads the number as an
 *  integer, so `MUL-007` and `MUL-7` are the same issue. */
function namesSameIssue(issue: Issue, identifier: string): boolean {
  const parts = IDENTIFIER_PARTS.exec(identifier);
  return (
    parts !== null &&
    issue.identifier.startsWith(`${parts[1]}-`) &&
    issue.number === Number(parts[2])
  );
}

/**
 * Resolve a bare issue identifier ("MUL-123") to a real issue in the current
 * workspace, or `null`. Backs the Linear-style autolink render path.
 *
 * Server state → TanStack Query (key includes wsId + identifier, so identical
 * identifiers across many comments/messages share one request).
 *
 * The query is short-circuited (no network) when:
 *   - there is no current workspace, or
 *   - the token is not identifier-shaped, or
 *   - the identifier's prefix cannot match the workspace's `issue_prefix`.
 *
 * When the prefix is unknown (workspace list still loading) we fall through to
 * the query and let the exact-match filter in `issueIdentifierOptions` decide.
 *
 * Returns `undefined` while the lookup is in flight, `null` once it settled
 * without an issue (404, exhausted retries, or short-circuited); a lookup
 * paused offline is still `undefined`, and the issue
 * otherwise. Callers reserve space for `undefined` and render plain text for
 * `null`.
 */
export function useResolveIssueIdentifier(identifier: string): Issue | null | undefined {
  const workspace = useCurrentWorkspace();
  const wsId = workspace?.id ?? "";
  const prefix = workspace?.issue_prefix;
  const prefixMatches =
    !prefix ||
    identifier.toUpperCase().startsWith(`${prefix.toUpperCase()}-`);

  const enabled = Boolean(wsId) && isIssueIdentifier(identifier) && prefixMatches;
  const { data, isPending } = useQuery({
    ...issueIdentifierOptions(wsId, identifier),
    enabled,
    // Keep known content across a key change, but only the same issue: the
    // previous result of a different issue or workspace must never paint.
    placeholderData: (previous) =>
      previous && previous.workspace_id === wsId && namesSameIssue(previous, identifier)
        ? keepPreviousData(previous)
        : undefined,
  });

  if (data) return data;
  // `isPending`, not `isLoading`: a lookup paused while offline is unresolved
  // too. A disabled query stays pending forever, so it must not count.
  return enabled && isPending ? undefined : null;
}
