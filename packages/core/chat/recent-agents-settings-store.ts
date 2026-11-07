"use client";

import { create } from "zustand";
import { createJSONStorage, persist } from "zustand/middleware";
import { createWorkspaceAwareStorage, registerForWorkspaceRehydration } from "../platform/workspace-storage";
import { defaultStorage } from "../platform/storage";

// Whether the New chat picker lists a "Recent" section of the agents the user
// DMs most. Per-workspace via the workspace-aware storage, like the
// create-issue field settings.
interface RecentAgentsSettingsState {
  showRecentAgents: boolean;
  setShowRecentAgents: (show: boolean) => void;
}

export const useRecentAgentsSettingsStore = create<RecentAgentsSettingsState>()(
  persist(
    (set) => ({
      showRecentAgents: true,
      setShowRecentAgents: (showRecentAgents) => set({ showRecentAgents }),
    }),
    {
      name: "multica_recent_agents_settings",
      storage: createJSONStorage(() => createWorkspaceAwareStorage(defaultStorage)),
      partialize: (s) => ({ showRecentAgents: s.showRecentAgents }),
      // Rehydrating a workspace with nothing stored must reset to the default,
      // not keep the previous workspace's value.
      merge: (persisted, current) => ({
        ...current,
        showRecentAgents:
          (persisted as Partial<RecentAgentsSettingsState> | undefined)?.showRecentAgents ?? true,
      }),
    },
  ),
);

registerForWorkspaceRehydration(() => useRecentAgentsSettingsStore.persist.rehydrate());
