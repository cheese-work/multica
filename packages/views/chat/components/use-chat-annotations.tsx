"use client";

import { useCallback, useMemo, useState } from "react";
import type { CommentDraftKey } from "@multica/core/issues/stores";
import type { ChatMessage } from "@multica/core/types";
import { useT } from "../../i18n";
import { useCommentAnnotations, type AnnotationLabels } from "../../issues/components/use-comment-annotations";
import type { ChatAnnotationScope } from "./chat-annotation-context";
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
  const active = enabled && !!sessionId;
  const draftKey: CommentDraftKey = `chat:${sessionId ?? ""}`;
  const [earlier, setEarlier] = useState<Record<string, string>>({});
  const registerSource = useCallback(
    (id: string, name: string) => setEarlier((prev) => (prev[id] === name ? prev : { ...prev, [id]: name })),
    [],
  );
  const sources = useMemo(
    () => [
      ...messages.filter((m) => m.role === "assistant").map((m) => ({ id: m.id, name: agentName ?? "" })),
      ...Object.entries(earlier).map(([id, name]) => ({ id, name })),
    ],
    [messages, agentName, earlier],
  );
  const labels: AnnotationLabels = {
    selectionFailed: t(($) => $.annotations.selection_failed),
    quoteLimit: (count) => t(($) => $.annotations.quote_limit, { count }),
    countLimit: (count) => t(($) => $.annotations.count_limit, { count }),
    add: t(($) => $.annotations.add),
    confirmAdd: t(($) => $.annotations.confirm_add),
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
