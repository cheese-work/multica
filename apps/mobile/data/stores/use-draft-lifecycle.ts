/**
 * Mounts the disk side of the draft stores for one workspace session
 * (`app/(app)/[workspace]/_layout.tsx`): loads the workspace's saved drafts
 * and writes pending ones when the app leaves the foreground, because the OS
 * may kill a backgrounded app before a debounced write fires.
 */
import { useEffect } from "react";
import { AppState } from "react-native";
import { draftPersistence } from "../draft-persistence";
import { useChatDraftsStore } from "./chat-drafts-store";

export function useDraftLifecycle(workspaceId: string | null) {
  useEffect(() => {
    if (workspaceId) void useChatDraftsStore.getState().hydrate(workspaceId);
  }, [workspaceId]);

  useEffect(() => {
    const sub = AppState.addEventListener("change", (state) => {
      if (state !== "active") void draftPersistence.flush();
    });
    return () => sub.remove();
  }, []);
}
