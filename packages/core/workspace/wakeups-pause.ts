import type { Workspace } from "../types";

export interface WakeupsPause {
  paused: boolean;
  /** User id the server stamped; empty when unknown. */
  by: string;
  /** ISO time the server stamped; empty when unknown. */
  at: string;
}

/** Reads the workspace wakeup pause (CHE-1727) from workspace.settings. */
export function deriveWakeupsPause(
  workspace: Pick<Workspace, "settings"> | null | undefined,
): WakeupsPause {
  const s = workspace?.settings ?? {};
  const text = (v: unknown) => (typeof v === "string" ? v : "");
  return {
    paused: s.wakeups_paused === true,
    by: text(s.wakeups_paused_by),
    at: text(s.wakeups_paused_at),
  };
}
