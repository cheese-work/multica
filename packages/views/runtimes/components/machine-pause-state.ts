import type {
  AgentRuntime,
  MemberRole,
  MemberWithUser,
  RuntimePause,
  RuntimePauseReason,
} from "@multica/core/types";

const MINUTE_MS = 60_000;
export const ENDING_SOON_MS = 5 * MINUTE_MS;

export type MachinePausePhase = "running" | "draining" | "drained";

export interface MachinePauseState {
  phase: MachinePausePhase;
  online: boolean;
  reason: RuntimePauseReason | null;
  /** Tasks running while the machine takes work; tasks left to finish once paused. */
  tasks: number;
  /** The live pause, or null when the machine takes work. */
  pause: RuntimePause | null;
  /** Time until the pause lifts; null when not paused or the server sent no usable expiry. */
  remainingMs: number | null;
  endingSoon: boolean;
}

function expiryMs(pause: RuntimePause): number | null {
  const ms = Date.parse(pause.expires_at);
  return Number.isNaN(ms) ? null : ms;
}

function isLive(pause: RuntimePause, now: number): boolean {
  const expiresAt = expiryMs(pause);
  return expiresAt === null || expiresAt > now;
}

/**
 * Collapse the per-runtime pause fields of one machine into what the card
 * and banner render. The server already hides an expired pause; the client
 * clock repeats that check so the card flips at the expiry without waiting
 * for a refetch.
 */
export function deriveMachinePauseState(
  runtimes: AgentRuntime[],
  options: { now: number; online: boolean; fallbackTasks: number },
): MachinePauseState {
  const { now, online, fallbackTasks } = options;
  const pause =
    runtimes.find((runtime) => runtime.pause && isLive(runtime.pause, now))?.pause ?? null;
  const counts = runtimes.flatMap((runtime) =>
    typeof runtime.daemon_running_tasks === "number" ? [runtime.daemon_running_tasks] : [],
  );
  const tasks = counts.length > 0 ? Math.max(...counts) : fallbackTasks;

  if (!pause) {
    return {
      phase: "running",
      online,
      reason: null,
      tasks,
      pause: null,
      remainingMs: null,
      endingSoon: false,
    };
  }
  const expiresAt = expiryMs(pause);
  const remainingMs = expiresAt === null ? null : expiresAt - now;
  return {
    phase: tasks > 0 ? "draining" : "drained",
    online,
    reason: pause.reason,
    tasks,
    pause,
    remainingMs,
    endingSoon: remainingMs !== null && remainingMs < ENDING_SOON_MS,
  };
}

/**
 * The pause that just ended by itself: the last pause this page saw, now past
 * its expiry, with the machine taking work again. A Resume lifts the pause
 * before its expiry, so it does not produce a notice.
 */
export function endedPauseNotice(
  lastSeen: RuntimePause | null,
  state: MachinePauseState,
  now: number,
): RuntimePause | null {
  if (!lastSeen || state.phase !== "running") return null;
  const expiresAt = expiryMs(lastSeen);
  return expiresAt !== null && expiresAt <= now ? lastSeen : null;
}

/** Whole minutes, rounded up, so "Ends in 0 min" never shows while time is left. */
export function remainingParts(ms: number): { hours: number; minutes: number } {
  const total = Math.max(1, Math.ceil(ms / MINUTE_MS));
  return { hours: Math.floor(total / 60), minutes: total % 60 };
}

/**
 * The runtime to address Pause and Resume to, or null when the viewer has no
 * right. The server allows the owner of the addressed runtime and the
 * workspace owner; it stays the real gate.
 */
export function pauseTarget(
  runtimes: AgentRuntime[],
  currentUserId: string | undefined,
  role: MemberRole | undefined,
): AgentRuntime | null {
  if (!currentUserId) return null;
  const owned = runtimes.filter((runtime) => runtime.owner_id === currentUserId);
  const pool = owned.length > 0 ? owned : role === "owner" ? runtimes : [];
  return pool.find((runtime) => runtime.status === "online") ?? pool[0] ?? null;
}

/** Names to ask when the viewer cannot pause: the machine owner, then workspace owners. */
export function pauseRightsHolders(
  runtimes: AgentRuntime[],
  members: MemberWithUser[],
): string[] {
  const ownerIds = new Set(runtimes.flatMap((runtime) => (runtime.owner_id ? [runtime.owner_id] : [])));
  const machineOwners = members.filter((member) => ownerIds.has(member.user_id));
  const workspaceOwners = members.filter((member) => member.role === "owner");
  return [...new Set([...machineOwners, ...workspaceOwners].map((member) => member.name))];
}
