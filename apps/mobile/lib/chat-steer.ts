import type { ChatHeadSupplement } from "@multica/core/types";

export interface SteerInput {
  /** Status of the running head task (`pendingTask.status`). */
  headStatus: string | undefined;
  /** Absent (an older server) reads as unsupported; `starting` waits. */
  headSupplement: ChatHeadSupplement | undefined;
  /** The caller may no longer run this agent. */
  noPermission?: boolean;
}

export interface SteerPresentation {
  canSteer: boolean;
  /** Key in the `chat` namespace explaining why Steer is off. */
  reasonKey: string | null;
}

/**
 * Whether a queued DM follow-up can be added to the running reply, and why
 * not. A starting run is waited on, never labelled unsupported. Mirrors the
 * rules in packages/views/chat/components/chat-queue.tsx.
 */
export function presentSteer({ headStatus, headSupplement, noPermission = false }: SteerInput): SteerPresentation {
  const dispatchableHead =
    headStatus === "dispatched" || headStatus === "running" || headStatus === "waiting_local_directory";
  if (!noPermission && dispatchableHead && headSupplement === "capable") {
    return { canSteer: true, reasonKey: null };
  }
  const reasonKey = noPermission
    ? "steer.no_permission"
    : !dispatchableHead
      ? "steer.unavailable"
      : headSupplement === "starting"
        ? "steer.starting"
        : "steer.unsupported";
  return { canSteer: false, reasonKey };
}

export interface SteerDeps {
  steer: (sessionId: string, taskId: string) => Promise<unknown>;
  /** Refetch messages and the pending task. Runs on success and failure. */
  refresh: (sessionId: string) => void;
}

/**
 * Adds a queued follow-up to the running reply. A failed steer changes
 * nothing server-side, so the follow-up stays queued; the refresh picks up
 * whatever made it fail (run ended, support changed).
 */
export async function steerQueuedTask(
  sessionId: string,
  taskId: string,
  { steer, refresh }: SteerDeps,
): Promise<"steered" | "failed"> {
  // Optimistic ids exist only on this device until the send is accepted.
  if (taskId.startsWith("optimistic-")) return "failed";
  try {
    await steer(sessionId, taskId);
    return "steered";
  } catch {
    return "failed";
  } finally {
    refresh(sessionId);
  }
}

export const FOLLOW_UP_ROW_HEIGHT = 32;
export const FOLLOW_UP_MAX_VISIBLE_ROWS = 3;

/**
 * Height cap for the queued follow-up list. The list sits above the composer,
 * outside the message scroller, so it must scroll instead of growing.
 */
export function followUpListMaxHeight(taskCount: number): number {
  return Math.min(taskCount, FOLLOW_UP_MAX_VISIBLE_ROWS) * FOLLOW_UP_ROW_HEIGHT;
}
