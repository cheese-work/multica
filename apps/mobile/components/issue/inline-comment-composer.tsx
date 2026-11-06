/**
 * Inline issue-comment composer — thin wrapper around the shared
 * `<MessageComposer>` with comment-specific wiring:
 *
 *   - `onSubmit` → `useCreateComment(issueId).mutateAsync`
 *   - Reply target sourced from `useReplyTargetStore` (set by the
 *     comment long-press action sheet); saved in the issue's composer
 *     draft so it survives leaving the issue
 *   - Mention picker path → `/[workspace]/mention-picker?mode=comment`
 *   - Upload context binds attachments to this issue
 *
 * All UI / state / chip plumbing lives in `MessageComposer`. The chat
 * composer (`components/chat/chat-composer.tsx`) uses the same component
 * with chat-mode props.
 */
import { useCallback, useEffect } from "react";
import { useCreateComment } from "@/data/mutations/issues";
import { useComposerDraftsStore } from "@/data/stores/composer-drafts-store";
import { useReplyTargetStore } from "@/data/stores/reply-target-store";
import { useWorkspaceStore } from "@/data/workspace-store";
import { MessageComposer } from "@/components/composer/message-composer";
import { useT } from "@/lib/i18n";

export function InlineCommentComposer({ issueId }: { issueId: string }) {
  const createComment = useCreateComment(issueId);
  const { t } = useT("issues");
  const wsSlug = useWorkspaceStore((s) => s.currentWorkspaceSlug);
  const replyTarget = useReplyTargetStore((s) => s.target);
  const clearReplyTarget = useReplyTargetStore((s) => s.clear);
  const draftKey = `comment:${issueId}`;
  const draftsHydrated = useComposerDraftsStore((s) => s.hydrated);

  // The reply store is the live copy (the long-press sheet writes to it).
  // Load it from this issue's draft, mirror later changes back, and empty it
  // on leaving. Unsubscribe before emptying so that is not saved as a clear.
  // A restored target must not pop the keyboard: only a target set after
  // the restore triggers expand + focus.
  useEffect(() => {
    const saved =
      useComposerDraftsStore.getState().drafts[draftKey]?.reply ?? null;
    useReplyTargetStore.setState({
      target: saved && { ...saved, restored: true },
    });
    const unsubscribe = useReplyTargetStore.subscribe((state, prev) => {
      if (state.target !== prev.target) {
        useComposerDraftsStore
          .getState()
          .patch(draftKey, { reply: state.target });
      }
    });
    return () => {
      unsubscribe();
      useReplyTargetStore.getState().clear();
    };
  }, [draftKey, draftsHydrated]);

  const onSubmit = useCallback(
    async ({
      content,
      attachmentIds,
    }: {
      content: string;
      attachmentIds: string[];
    }) => {
      try {
        await createComment.mutateAsync({
          content,
          parentId: replyTarget?.commentId,
          attachmentIds: attachmentIds.length > 0 ? attachmentIds : undefined,
        });
      } catch (err) {
        // Rethrow so MessageComposer's catch path restores text + chips.
        // The optimistic timeline row stays with its inline
        // Failed · Retry · Discard affordance.
        throw err;
      }
    },
    [createComment, replyTarget],
  );

  return (
    <MessageComposer
      draftKey={draftKey}
      onSubmit={onSubmit}
      mentionPickerPath={{
        pathname: "/[workspace]/mention-picker",
        params: { workspace: wsSlug ?? "", mode: "comment" },
      }}
      uploadContext={{ issueId }}
      placeholder={t("comments.composer_placeholder")}
      pillLabel={t("comments.composer_pill")}
      pillIcon="chatbubble-ellipses-outline"
      replyTarget={
        replyTarget
          ? {
              actorName: replyTarget.actorName,
              preview: replyTarget.preview,
            }
          : null
      }
      onClearReplyTarget={clearReplyTarget}
      expandTrigger={
        replyTarget && !replyTarget.restored ? replyTarget.commentId : null
      }
    />
  );
}
