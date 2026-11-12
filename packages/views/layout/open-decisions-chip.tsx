"use client";

import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { paths, useWorkspaceSlug } from "@multica/core/paths";
import { openDecisionRequestsOptions } from "@multica/core/chat/queries";
import { useT } from "../i18n";
import { AppLink } from "../navigation";

/**
 * Top-bar chip, on every page, while a person has an open decision. A click
 * opens the oldest one: the issue at its card comment, else the issue.
 */
export function OpenDecisionsChip() {
  const wsId = useWorkspaceId();
  const slug = useWorkspaceSlug();
  const { t } = useT("chat");
  const { data } = useQuery(openDecisionRequestsOptions(wsId));
  if (!slug || !data?.length) return null;
  const oldest = [...data].sort((a, b) => Date.parse(a.created_at) - Date.parse(b.created_at))[0]!;
  const issue = paths.workspace(slug).issueDetail(oldest.issue_id);
  const href = oldest.source_comment_id ? `${issue}#comment-${oldest.source_comment_id}` : issue;
  return (
    <AppLink
      href={href}
      className="mx-auto mt-2 flex min-h-9 w-fit items-center rounded-lg border border-warning/40 bg-warning/10 px-3 text-caption font-medium text-warning focus-visible:outline-2 focus-visible:outline-ring"
    >
      {t(($) => $.needs_you.chip, { count: data.length })}
    </AppLink>
  );
}
