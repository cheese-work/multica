"use client";

import { create } from "zustand";
import { createJSONStorage, persist } from "zustand/middleware";
import { createWorkspaceAwareStorage, registerForWorkspaceRehydration } from "../platform/workspace-storage";
import { defaultStorage } from "../platform/storage";

// Whether the New chat picker shows its "Recent" agents. Saved per workspace,
// like the create-issue field settings.
interface RecentAgentsSettingsState {
  showRecentAgents: boolean;
  setShowRecentAgents: (value: boolean) => void;
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
      // A workspace with nothing saved must fall back to the default rather
      // than keep the previous workspace's value in memory.
      merge: (saved, current) => {
        const value = (saved as Partial<RecentAgentsSettingsState> | null | undefined)?.showRecentAgents;
        return { ...current, showRecentAgents: typeof value === "boolean" ? value : true };
      },
    },
  ),
);

registerForWorkspaceRehydration(() => useRecentAgentsSettingsStore.persist.rehydrate());
