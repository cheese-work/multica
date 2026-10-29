"use client";

import type { ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { commentPreviewOptions } from "@multica/core/issues/queries";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { AppLink } from "../../navigation";
import { IssueChip } from "./issue-chip";

/**
 * Navigable chip for a comment reference: `CHE-1 · <author>: <first line>`,
 * linking to the comment on its issue page.
 *
 * Until the server resolves the id — and for good when it will not (unknown,
 * deleted, another workspace) — it renders `fallback` (the text the author wrote), so a
 * comment the viewer cannot read never shows a preview and a miss looks exactly
 * like the text the author wrote.
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
  const { data } = useQuery(commentPreviewOptions(wsId, commentId));

  if (!data) return <>{fallback}</>;

  const author = data.author_name ? `${data.author_name}: ` : "";
  return (
    <AppLink
      href={`${p.issueDetail(data.issue_id)}#comment-${data.comment_id}`}
      newTabTitle={data.issue_identifier}
      className="issue-mention align-middle"
    >
      <IssueChip
        issueId={data.issue_id}
        className="cursor-pointer hover:bg-accent transition-colors"
      >
        <span className="font-medium text-muted-foreground shrink-0">
          {data.issue_identifier} ·
        </span>
        <span className="min-w-0 truncate text-foreground">
          {author}
          {data.preview}
        </span>
      </IssueChip>
    </AppLink>
  );
}
