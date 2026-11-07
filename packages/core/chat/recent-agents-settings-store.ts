import { create } from "zustand";

// Inert stub: in memory only, nothing saved yet.
interface RecentAgentsSettingsState {
  showRecentAgents: boolean;
  setShowRecentAgents: (value: boolean) => void;
}

export const useRecentAgentsSettingsStore = Object.assign(
  create<RecentAgentsSettingsState>()((set) => ({
    showRecentAgents: true,
    setShowRecentAgents: (showRecentAgents) => set({ showRecentAgents }),
  })),
  { persist: { rehydrate: async () => {} } },
);
