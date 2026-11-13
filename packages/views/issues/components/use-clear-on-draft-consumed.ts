"use client";

import { useEffect, useRef, type RefObject } from "react";
import { useCommentDraftStore, type CommentDraftKey } from "@multica/core/issues/stores";
import type { ContentEditorRef } from "../../editor";

/**
 * Follow a draft that another composer instance consumed (CHE-4288).
 *
 * A page remount mid-send (the route rewrites /issues/<uuid> to the identifier
 * URL) leaves two instances for one draft key: the unmounted one whose send
 * succeeds and clears the store, and the new one that mounted from the still
 * unsent draft. Only the new instance has an editor to clear. It does so when
 * the entry vanishes while the editor still shows exactly what the entry held;
 * a user who emptied or changed the text themselves is left alone.
 */
export function useClearOnDraftConsumed(
  draftKey: CommentDraftKey | undefined,
  editorRef: RefObject<ContentEditorRef | null>,
  onCleared: () => void,
) {
  const onClearedRef = useRef(onCleared);
  onClearedRef.current = onCleared;
  useEffect(() => {
    if (!draftKey) return;
    return useCommentDraftStore.subscribe((state, prev) => {
      // Read the entries, not getDraft(): that accessor reads the live store.
      const held = prev.drafts[draftKey]?.content;
      if (!held || state.drafts[draftKey] !== undefined) return;
      const shown = editorRef.current?.getMarkdown()?.trim();
      if (shown !== held.trim()) return;
      editorRef.current?.clearContent();
      onClearedRef.current();
    });
  }, [draftKey, editorRef]);
}
