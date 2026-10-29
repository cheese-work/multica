"use client";

import type { ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { commentPreviewOptions } from "@multica/core/issues/queries";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { AppLink } from "../../navigation";
import { ISSUE_CHIP_CLASS } from "./issue-chip";

/**
 * Navigable chip for a comment reference: `CHE-1 · <author>: <first line>`,
 * linking to the comment on its issue page.
 *
 * While the server resolves the id, `fallback` (the text the author wrote)
 * sits inside the chip's own box with a transparent border, so resolving swaps
 * content without changing the box the line was laid out around. When the
 * server will not resolve it (unknown, deleted, another workspace, or a refetch
 * that now fails) it renders bare `fallback`, so a comment the viewer cannot
 * read never shows a preview.
 */
export function CommentMentionCard({
  commentId,
  fallback,
}: {
  commentId: string;
  fallback: ReactNode;
}) {
  const wsId = useWorkspaceId();
  const p = useWorkspacePaths();
  const { data, isError } = useQuery(commentPreviewOptions(wsId, commentId));

  if (isError) return <>{fallback}</>;
  if (!data) {
    return <span className={`${ISSUE_CHIP_CLASS} border-transparent`}>{fallback}</span>;
  }

  const author = data.author_name ? `${data.author_name}: ` : "";
  return (
    <AppLink
      href={`${p.issueDetail(data.issue_id)}#comment-${data.comment_id}`}
      newTabTitle={data.issue_identifier}
      className="issue-mention align-middle"
    >
      <span className={`${ISSUE_CHIP_CLASS} cursor-pointer hover:bg-accent transition-colors`}>
        <span className="font-medium text-muted-foreground shrink-0">
          {data.issue_identifier} ·
        </span>
        <span className="min-w-0 truncate text-foreground">
          {author}
          {data.preview}
        </span>
      </span>
    </AppLink>
  );
}
