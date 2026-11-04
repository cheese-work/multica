/**
 * Mounts the disk side of the draft stores for one workspace session
 * (`app/(app)/[workspace]/_layout.tsx`): loads the workspace's saved drafts
 * and writes pending ones when the app leaves the foreground, because the OS
 * may kill a backgrounded app before a debounced write fires.
 *
 * Drafts are scoped by user AND workspace. If logout cannot delete the old
 * account's drafts from disk, the next account still never loads them.
 */
import { useEffect } from "react";
import { AppState } from "react-native";
import { draftPersistence } from "../draft-persistence";
import { useChatDraftsStore } from "./chat-drafts-store";
import { useComposerDraftsStore } from "./composer-drafts-store";

export function useDraftLifecycle(
  userId: string | null,
  workspaceId: string | null,
) {
  useEffect(() => {
    if (userId && workspaceId) {
      const scope = `${userId}:${workspaceId}`;
      void useChatDraftsStore.getState().hydrate(scope);
      void useComposerDraftsStore.getState().hydrate(scope);
    }
  }, [userId, workspaceId]);

  useEffect(() => {
    const sub = AppState.addEventListener("change", (state) => {
      if (state !== "active") void draftPersistence.flush();
    });
    return () => sub.remove();
  }, []);
}
