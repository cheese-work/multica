"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { useCommentDraftStore, type CommentDraftKey } from "@multica/core/issues/stores";
import type { ChatMessage } from "@multica/core/types";
import { useT } from "../../i18n";
import { useCommentAnnotations, type AnnotationLabels } from "../../issues/components/use-comment-annotations";
import { DECISION_REQUEST_SOURCE_PREFIX, type ChatAnnotationScope } from "./chat-annotation-context";
import type { ChatInputAnnotations } from "./chat-annotations";

/**
 * Comments on agent replies in one DM (CHE-1876). Wraps the issue annotation
 * hook for the chat surface: the draft lives under `chat:<sessionId>` and the
 * quotes ride along with the next message. Mount `cardRef` and `captureProps`
 * on an element that holds both the message list and the composer.
 */
export function useChatAnnotations({ sessionId, messages, agentName, enabled }: {
  sessionId: string | null;
  messages: ChatMessage[];
  agentName?: string;
  /** False for a read-only DM: nothing to send the comments with. */
  enabled: boolean;
}) {
  const { t } = useT("chat");
  // Same labels as the issue page for the two controls that open and confirm a comment.
  const { t: tIssues } = useT("issues");
  const active = enabled && !!sessionId;
  const draftKey: CommentDraftKey = `chat:${sessionId ?? ""}`;
  const annotations = useCommentDraftStore((state) => state.getAnnotations(draftKey));
  const firstSource = annotations[0]?.sourceCommentId;
  // A comment on a decision card that was answered or withdrawn is stale: left in
  // the draft it would route every later send as a reply to that card, or block
  // it as a mixed request. Only a card seen closed counts; an unloaded one stays.
  useEffect(() => {
    const closed = new Set(messages.flatMap((message) =>
      (message.decision_requests ?? (message.decision_request ? [message.decision_request] : []))
        .filter((card) => card.state !== "open").map((card) => `${DECISION_REQUEST_SOURCE_PREFIX}${card.id}`)));
    for (const a of annotations) if (closed.has(a.sourceCommentId)) useCommentDraftStore.getState().removeAnnotation(draftKey, a.id);
  }, [annotations, messages, draftKey]);
  const [earlier, setEarlier] = useState<Record<string, string>>({});
  const registerSource = useCallback(
    (id: string, name: string) => setEarlier((prev) => (prev[id] === name ? prev : { ...prev, [id]: name })),
    [],
  );
  const sources = useMemo(
    () => [
      ...messages.filter((message) => message.role === "assistant").flatMap((message) => {
        const requests = message.decision_requests ?? (message.decision_request ? [message.decision_request] : []);
        if (requests.length) return requests.filter((card) => card.state === "open").map((card) => ({ id: `${DECISION_REQUEST_SOURCE_PREFIX}${card.id}`, name: agentName ?? "" }));
        return [{ id: message.id, name: agentName ?? "" }];
      }),
      ...Object.entries(earlier).map(([id, name]) => ({ id, name })),
    ].filter((source) => !firstSource || (firstSource.startsWith(DECISION_REQUEST_SOURCE_PREFIX)
      ? source.id === firstSource : !source.id.startsWith(DECISION_REQUEST_SOURCE_PREFIX))),
    [messages, agentName, earlier, firstSource],
  );
  const labels: AnnotationLabels = {
    selectionFailed: t(($) => $.annotations.selection_failed),
    quoteLimit: (count) => t(($) => $.annotations.quote_limit, { count }),
    countLimit: (count) => t(($) => $.annotations.count_limit, { count }),
    add: tIssues(($) => $.reply.annotations.add),
    confirmAdd: tIssues(($) => $.reply.annotations.confirm_add),
    save: t(($) => $.annotations.save),
    noteLabel: (number) => t(($) => $.annotations.note_label, { number }),
    notePlaceholder: t(($) => $.annotations.note_placeholder),
    edit: (number, note) => t(($) => $.annotations.edit, { number, note }),
    remove: (number) => t(($) => $.annotations.remove, { number }),
  };
  const annotation = useCommentAnnotations({ draftKey, sources, enabled: active, surface: "chat", labels });
  const scope = useMemo<ChatAnnotationScope>(
    () => ({ enabled: active, agentName, registerSource }),
    [active, agentName, registerSource],
  );
  const input: ChatInputAnnotations | undefined = active
    ? {
        draftKey,
        onEditAnnotation: (id) => annotation.editAnnotation(id, true),
        activeId: annotation.activeId,
        onActiveChange: annotation.setActiveId,
      }
    : undefined;
  return { cardRef: annotation.cardRef, captureProps: annotation.captureProps, popup: annotation.popup, scope, input };
}
