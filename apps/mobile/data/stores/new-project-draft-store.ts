/**
 * Draft state for the New Project modal (`app/(app)/[workspace]/project/new.tsx`).
 *
 * Mirrors `new-issue-draft-store.ts` — same rationale: the formSheet picker
 * routes (`new-project-picker/status.tsx`, `new-project-picker/priority.tsx`)
 * live in a separate Stack screen and have no React parent-child relationship
 * with the new-project modal. They need a way to read the current draft value
 * and write the new selection back without prop-drilling through the router.
 * A small Zustand store is the minimum-viable cross-screen channel — and the
 * same pattern as new-issue.
 *
 * Lifecycle: `reset()` runs from `project/new.tsx` after a successful create
 * or an explicit discard. Closing the modal keeps the chips: the screen saves
 * them with the title, icon and description through `form-drafts-store` and
 * restores them on the next open (also after an app cold start).
 *
 * Workspace lifecycle: workspace-scoped — reset is wired in
 * `app/(app)/[workspace]/_layout.tsx` via
 * `useNewProjectDraftResetOnWorkspaceChange()`, the only caller.
 */
import { useEffect, useRef } from "react";
import { create } from "zustand";
import { registerDraftCleanup } from "../draft-persistence";
import type { ProjectPriority, ProjectStatus } from "@multica/core/types";

interface NewProjectDraftState {
  status: ProjectStatus;
  priority: ProjectPriority;
  setStatus: (next: ProjectStatus) => void;
  setPriority: (next: ProjectPriority) => void;
  reset: () => void;
}

const INITIAL: Pick<NewProjectDraftState, "status" | "priority"> = {
  status: "planned",
  priority: "none",
};

export const useNewProjectDraftStore = create<NewProjectDraftState>((set) => ({
  ...INITIAL,
  setStatus: (next) => set({ status: next }),
  setPriority: (next) => set({ priority: next }),
  reset: () => set({ ...INITIAL }),
}));

// Logout: the next account must not inherit this account's picker choices.
registerDraftCleanup(() => useNewProjectDraftStore.getState().reset());

/**
 * Clears the new-project draft store whenever the active workspace id
 * changes. The previous draft is invalid in a different workspace (we may
 * later add fields like `lead` whose id only resolves in the seeded
 * workspace). The `useRef` gate ensures the first mount is a no-op — we
 * only fire `reset()` when the id actually changes, not on the initial
 * render where `prev === current`.
 */
export function useNewProjectDraftResetOnWorkspaceChange(wsId: string | null) {
  const prevRef = useRef(wsId);
  useEffect(() => {
    if (prevRef.current !== wsId) {
      useNewProjectDraftStore.getState().reset();
      prevRef.current = wsId;
    }
  }, [wsId]);
}
