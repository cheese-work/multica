/**
 * Workspace deleted: its unsent drafts are meaningless, so drop them from
 * memory and disk. Listing-level (mounted for the workspace session) because
 * the event can arrive on any screen.
 */
import { useAuthStore } from "@/data/auth-store";
import { clearScopeDrafts } from "@/data/draft-persistence";
import { useWSSubscriptions } from "@/lib/use-ws-subscriptions";

export function useWorkspaceDraftsRealtime() {
  useWSSubscriptions(
    (ws) => [
      ws.on("workspace:deleted", ({ workspace_id }) => {
        const userId = useAuthStore.getState().user?.id;
        if (userId) void clearScopeDrafts(`${userId}:${workspace_id}`);
      }),
    ],
    [],
  );
}
