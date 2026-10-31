"use client";

import { z } from "zod";
import { create } from "zustand";
import { createJSONStorage, persist, type StateStorage } from "zustand/middleware";
import { registerDraftCleanup } from "../drafts/cleanup-registry";
import { defaultStorage } from "../platform/storage";
import type { AffectedMachine } from "../types";

export const AffectedMachineSchema = z.object({
  machine_owner_id: z.string(),
  daemon_id: z.string(),
  device_name: z.string().catch(""),
  state: z.enum(["paused", "offline"]),
  reason: z.enum(["maintenance", "incident", "suspended"]).nullable().catch(null),
  event_id: z.string(),
  expires_at: z.string().nullable().catch(null),
  waiting_tasks: z.number().int().nonnegative().catch(0),
}).loose();

/**
 * A machine in a state this client does not know (a newer server) is dropped,
 * not shown as paused or offline: a wrong cause is worse than no banner.
 */
export const AffectedMachinesSchema = z.object({
  machines: z
    .array(z.unknown())
    .catch([])
    .transform((items) =>
      items.flatMap((item) => {
        const parsed = AffectedMachineSchema.safeParse(item);
        return parsed.success ? [parsed.data as AffectedMachine] : [];
      }),
    ),
});

export const EMPTY_AFFECTED_MACHINES: { machines: AffectedMachine[] } = { machines: [] };

/** Bound on remembered dismissals; an event older than this is long over. */
const MAX_DISMISSED = 50;

export interface DismissedMachineEventsState {
  /** Event ids the viewer closed, oldest first. */
  dismissed: string[];
  dismiss: (eventIds: string[]) => void;
  reopen: (eventIds: string[]) => void;
}

const stateStorage = defaultStorage as unknown as StateStorage;

export const useDismissedMachineEventsStore = create<DismissedMachineEventsState>()(
  persist(
    (set) => ({
      dismissed: [],
      dismiss: (eventIds) =>
        set((state) => ({
          dismissed: [
            ...state.dismissed.filter((id) => !eventIds.includes(id)),
            ...eventIds,
          ].slice(-MAX_DISMISSED),
        })),
      reopen: (eventIds) =>
        set((state) => ({
          dismissed: state.dismissed.filter((id) => !eventIds.includes(id)),
        })),
    }),
    {
      name: "multica_dismissed_machine_events",
      storage: createJSONStorage(() => stateStorage),
    },
  ),
);

// Dismissals are per viewer; ending a session wipes them like it wipes drafts.
registerDraftCleanup({
  storageKey: "multica_dismissed_machine_events",
  workspaceScoped: false,
  resetInMemory: () => useDismissedMachineEventsStore.setState({ dismissed: [] }),
});

/** Details open/closed, so reopening the banner from the header icon keeps it as it was. */
export const useMachineBannerDetailsStore = create<{
  open: boolean;
  setOpen: (open: boolean) => void;
}>((set) => ({ open: false, setOpen: (open) => set({ open }) }));
