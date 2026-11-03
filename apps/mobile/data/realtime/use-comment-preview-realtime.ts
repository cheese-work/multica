/**
 * Comment-ID chip previews — listing-level. A chip can point at a comment in
 * any issue, so this subscription is workspace-wide rather than tied to the
 * open issue. An edit or delete refetches that one preview (a delete settles
 * to plain text via the endpoint's 404); a reconnect refetches all of them
 * because events may have been missed.
 */
import { useQueryClient } from "@tanstack/react-query";
import { issueKeys } from "@/data/queries/issue-keys";
import { useWSSubscriptions } from "@/lib/use-ws-subscriptions";

export function useCommentPreviewRealtime() {
  const qc = useQueryClient();

  useWSSubscriptions(
    (ws, wsId) => {
      const refresh = (commentId: string | undefined) => {
        if (commentId) {
          qc.invalidateQueries({ queryKey: issueKeys.commentPreview(wsId, commentId) });
        }
      };
      return [
        ws.on("comment:updated", (p) => refresh(p.comment.id)),
        ws.on("comment:deleted", (p) => refresh(p.comment_id)),
        ws.onReconnect(() =>
          qc.invalidateQueries({
            predicate: ({ queryKey }) =>
              queryKey[0] === "issues" &&
              queryKey[1] === wsId &&
              queryKey[2] === "comment-preview",
          }),
        ),
      ];
    },
    [qc],
  );
}
