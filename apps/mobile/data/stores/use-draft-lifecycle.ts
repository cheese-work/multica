/**
 * Mounts the disk side of the draft stores for one workspace session
 * (`app/(app)/[workspace]/_layout.tsx`): loads the workspace's saved drafts
 * and writes pending ones when the app leaves the foreground, because the OS
 * may kill a backgrounded app before a debounced write fires.
 *
 * `workspaces` is the user's workspace list, passed only once it is fetched
 * and settled (null otherwise). Drafts of a workspace missing from it were
 * deleted while the device was offline and are dropped.
 *
 * Drafts are scoped by user AND workspace. If logout cannot delete the old
 * account's drafts from disk, the next account still never loads them.
 */
import { useEffect } from "react";
import { AppState } from "react-native";
import {
  draftPersistence,
  reconcileWorkspaceDrafts,
} from "../draft-persistence";
import { useChatDraftsStore } from "./chat-drafts-store";
import { useFormDraftsStore } from "./form-drafts-store";
import { useComposerDraftsStore } from "./composer-drafts-store";
import { usePendingSendStore } from "./pending-send-store";

export function useDraftLifecycle(
  userId: string | null,
  workspaceId: string | null,
  workspaces: readonly { id: string }[] | null,
) {
  useEffect(() => {
    if (userId && workspaceId) {
      const scope = `${userId}:${workspaceId}`;
      void useChatDraftsStore.getState().hydrate(scope);
      void useFormDraftsStore.getState().hydrate(scope);
      void useComposerDraftsStore.getState().hydrate(scope);
      void usePendingSendStore.getState().hydrate(scope);
    }
  }, [userId, workspaceId]);

  useEffect(() => {
    if (userId && workspaces) {
      reconcileWorkspaceDrafts(userId, workspaces).catch((err) =>
        console.warn("[drafts] workspace reconcile failed", err),
      );
    }
  }, [userId, workspaces]);

  useEffect(() => {
    const sub = AppState.addEventListener("change", (state) => {
      if (state !== "active") void draftPersistence.flush();
    });
    return () => sub.remove();
  }, []);
}
