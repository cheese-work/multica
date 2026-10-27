"use client";

import { create } from "zustand";
import { createJSONStorage, persist } from "zustand/middleware";
import { defaultStorage } from "../../platform/storage";
import type { RelationFilter } from "../relations";

// Edge types the user hid in the Relations graph. Per-user via browser-local
// storage, not per-workspace: it is a viewing preference. Mentions start off
// because they dominate most graphs.
interface RelationsFilterState {
  hidden: RelationFilter[];
  toggle: (filter: RelationFilter) => void;
}

export const useRelationsFilterStore = create<RelationsFilterState>()(
  persist(
    (set) => ({
      hidden: ["mention"],
      toggle: (filter) => set((state) => ({ hidden: state.hidden.includes(filter) ? state.hidden.filter((item) => item !== filter) : [...state.hidden, filter] })),
    }),
    { name: "multica_relations_filter", storage: createJSONStorage(() => defaultStorage) },
  ),
);
