"use client";

import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { paths, useWorkspaceSlug } from "@multica/core/paths";
import { openDecisionRequestsOptions } from "@multica/core/chat/queries";
import { useT } from "../i18n";
import { AppLink, useNavigation } from "../navigation";

/**
 * Top-bar chip, on every page, while a person has an open decision. A click
 * opens the oldest one: the issue at its card comment, else the issue. Not on
 * the chat route: the DM header carries the same count as a badge there.
 */
export function OpenDecisionsChip() {
  const wsId = useWorkspaceId();
  const slug = useWorkspaceSlug();
  const { t } = useT("chat");
  const { pathname } = useNavigation();
  const { data } = useQuery(openDecisionRequestsOptions(wsId));
  if (!slug || !data?.length || pathname.startsWith(paths.workspace(slug).chat())) return null;
  const oldest = [...data].sort((a, b) => Date.parse(a.created_at) - Date.parse(b.created_at))[0]!;
  // The identifier, not the UUID: a UUID link is rewritten to the identifier
  // after load, which remounts the issue page and throws the landing away.
  const issue = paths.workspace(slug).issueDetail(oldest.issue_identifier || oldest.issue_id);
  const href = oldest.source_comment_id ? `${issue}#comment-${oldest.source_comment_id}` : issue;
  return (
    <AppLink
      href={href}
      className="mx-auto mt-2 flex min-h-9 w-fit items-center rounded-full border border-warning/40 bg-warning/10 px-3 text-caption font-medium text-warning focus-visible:outline-2 focus-visible:outline-ring"
    >
      {t(($) => $.needs_you.chip, { count: data.length })}
    </AppLink>
  );
}
