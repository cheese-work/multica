"use client";

import { create } from "zustand";

// STUB (test-first red step): in-memory only, no persistence.
interface RecentAgentsSettingsState {
  showRecentAgents: boolean;
  setShowRecentAgents: (show: boolean) => void;
}

export const useRecentAgentsSettingsStore = Object.assign(
  create<RecentAgentsSettingsState>()((set) => ({
    showRecentAgents: true,
    setShowRecentAgents: (showRecentAgents) => set({ showRecentAgents }),
  })),
  { persist: { rehydrate: async () => {} } },
);
