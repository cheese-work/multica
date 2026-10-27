import { summarizeChatSessionUsage } from "@multica/core/chat/usage";
import type { ChatSession, ChatSessionUsage } from "@/data/schemas";
import type { ChatSessionActionKind } from "@/data/stores/chat-session-actions-store";

const TOKEN_UNITS = [
  { divisor: 1, suffix: "" },
  { divisor: 1_000, suffix: "K" },
  { divisor: 1_000_000, suffix: "M" },
  { divisor: 1_000_000_000, suffix: "B" },
  { divisor: 1_000_000_000_000, suffix: "T" },
] as const;

export function formatStatsUsd(n: number): string {
  if (n >= 100) return `$${n.toFixed(0)}`;
  return `$${n.toFixed(2)}`;
}

export function formatStatsTokens(n: number): string {
  const magnitude = Math.abs(n);
  let unitIndex = TOKEN_UNITS.findLastIndex(
    ({ divisor }) => magnitude >= divisor,
  );
  unitIndex = Math.max(unitIndex, 0);

  if (unitIndex === 0) return n.toLocaleString();

  let unit = TOKEN_UNITS[unitIndex]!;
  let scaled = n / unit.divisor;

  if (
    Math.abs(Number(scaled.toFixed(1))) >= 1_000 &&
    unitIndex < TOKEN_UNITS.length - 1
  ) {
    unit = TOKEN_UNITS[unitIndex + 1]!;
    scaled = n / unit.divisor;
  }

  return `${Number(scaled.toFixed(1))}${unit.suffix}`;
}

/**
 * Thrown when `handleSend` on an archived DM intercepts the send to present
 * the native confirmation alert (`mobile-archived-send-alert`).
 * `MessageComposer` catches this rejection, restores the draft text,
 * mentions, and attachments into the input, and keeps the composer expanded
 * so the user's draft is never lost while deciding.
 */
export class ArchivedSendInterceptedError extends Error {
  readonly isArchivedSendIntercepted = true;
  constructor() {
    super("Archived DM send intercepted for confirmation");
    this.name = "ArchivedSendInterceptedError";
  }
}

export function isArchivedSendIntercepted(err: unknown): boolean {
  return (
    err instanceof ArchivedSendInterceptedError ||
    (typeof err === "object" &&
      err !== null &&
      "isArchivedSendIntercepted" in err &&
      (err as { isArchivedSendIntercepted?: unknown }).isArchivedSendIntercepted === true)
  );
}

export interface ArchivedSendAlertButton {
  text: string;
  style?: "cancel";
  isPreferred?: boolean;
  onPress: () => void;
}

/**
 * Buttons for the archived-DM send alert. iOS lists them in display order.
 * React Native Android maps array order to [neutral, negative, positive] for
 * three buttons, so with a newer DM the order is flipped there: "Open the
 * newer DM" is the positive (primary) action, "Unarchive and send" the
 * negative (secondary), and Cancel the neutral one (A2).
 */
export function buildArchivedSendAlertButtons({
  platform,
  hasNewer,
  labels,
  onOpenNewer,
  onConfirm,
  onCancel,
}: {
  platform: string;
  hasNewer: boolean;
  labels: { openNewer: string; confirm: string; cancel: string };
  onOpenNewer: () => void;
  onConfirm: () => void;
  onCancel: () => void;
}): ArchivedSendAlertButton[] {
  const cancel: ArchivedSendAlertButton = {
    text: labels.cancel,
    style: "cancel",
    onPress: onCancel,
  };
  if (!hasNewer) {
    return [cancel, { text: labels.confirm, isPreferred: true, onPress: onConfirm }];
  }
  const openNewer: ArchivedSendAlertButton = {
    text: labels.openNewer,
    isPreferred: true,
    onPress: onOpenNewer,
  };
  const confirm: ArchivedSendAlertButton = { text: labels.confirm, onPress: onConfirm };
  return platform === "android"
    ? [cancel, confirm, openNewer]
    : [openNewer, confirm, cancel];
}

/**
 * Unarchive, then send exactly once. A failed unarchive rejects with
 * `ArchivedSendInterceptedError` without sending, so the composer keeps the draft.
 */
export async function unarchiveThenSendOnce(
  unarchive: () => Promise<boolean>,
  send: () => Promise<void>,
): Promise<void> {
  if (!(await unarchive())) throw new ArchivedSendInterceptedError();
  await send();
}

/**
 * Optimistic cache patch for archive/unarchive. Archived DMs are unpinned and
 * unarchiving never restores an old pin (A2).
 */
export function patchSessionArchived(session: ChatSession, archived: boolean): ChatSession {
  return {
    ...session,
    status: archived ? "archived" : "active",
    pinned: false,
    has_unread: archived ? false : session.has_unread,
    unread_count: archived ? 0 : session.unread_count,
  };
}

export function sessionActivityTime(session: ChatSession): string {
  const lastMsgAt = session.last_message?.created_at;
  if (lastMsgAt && lastMsgAt > session.updated_at) return lastMsgAt;
  return session.updated_at;
}

/**
 * Sorts active sessions: pinned first, then by activity descending.
 */
export function sortActiveChatSessions(sessions: readonly ChatSession[]): ChatSession[] {
  return [...sessions].sort((a, b) => {
    const aPinned = a.pinned ? 1 : 0;
    const bPinned = b.pinned ? 1 : 0;
    if (aPinned !== bPinned) return bPinned - aPinned;
    return sessionActivityTime(b).localeCompare(sessionActivityTime(a));
  });
}

/**
 * Sorts archived sessions strictly by activity descending, ignoring `pinned`
 * so archived rows never surface a pinned category or pin ordering.
 */
export function sortArchivedChatSessions(sessions: readonly ChatSession[]): ChatSession[] {
  return [...sessions].sort((a, b) =>
    sessionActivityTime(b).localeCompare(sessionActivityTime(a)),
  );
}

/**
 * Re-orders a flat `status=all` cache array so active sessions (pinned-first)
 * precede archived sessions (activity-only).
 */
export function sortAllChatSessions(sessions: readonly ChatSession[]): ChatSession[] {
  const { active, archived } = splitChatSessions(sessions);
  return [...active, ...archived];
}

export function splitChatSessions(sessions: readonly ChatSession[]): {
  active: ChatSession[];
  archived: ChatSession[];
} {
  const activeRaw: ChatSession[] = [];
  const archivedRaw: ChatSession[] = [];
  for (const s of sessions) {
    if (s.status === "archived") {
      archivedRaw.push(s);
    } else {
      activeRaw.push(s);
    }
  }
  return {
    active: sortActiveChatSessions(activeRaw),
    archived: sortArchivedChatSessions(archivedRaw),
  };
}

export interface ChatMoreMenuItemDescriptor {
  key: "statistics" | "pin" | "archive" | "delete";
  testID:
    | "mobile-chat-more-statistics"
    | "mobile-chat-more-pin"
    | "mobile-chat-more-archive"
    | "mobile-chat-more-delete";
  disabled: boolean;
  isPending?: boolean;
  destructive?: boolean;
}

/**
 * Builds the item descriptors for the reusable `ChatMoreMenu` (CEO-6).
 * - Active DM: Statistics, Pin/Unpin, Archive (disabled while pending). No Delete!
 * - Archived DM: Statistics, Unarchive (disabled while pending), Delete (destructive). No Pin!
 */
export function buildChatMoreMenuItems(opts: {
  session: ChatSession;
  actionPending: ChatSessionActionKind | null;
}): ChatMoreMenuItemDescriptor[] {
  const { session, actionPending } = opts;
  const isArchived = session.status === "archived";
  const isPending = actionPending !== null;

  const items: ChatMoreMenuItemDescriptor[] = [
    {
      key: "statistics",
      testID: "mobile-chat-more-statistics",
      disabled: false,
    },
  ];

  if (!isArchived) {
    items.push({
      key: "pin",
      testID: "mobile-chat-more-pin",
      disabled: isPending,
    });
  }

  items.push({
    key: "archive",
    testID: "mobile-chat-more-archive",
    disabled: isPending,
    isPending,
  });

  if (isArchived) {
    items.push({
      key: "delete",
      testID: "mobile-chat-more-delete",
      disabled: isPending,
      destructive: true,
    });
  }

  return items;
}

export type ChatStatisticsSummary = ReturnType<typeof summarizeChatSessionUsage>;

export interface DerivedChatStatisticsState {
  kind: "loading" | "error" | "empty" | "populated";
  stateTestId:
    | "mobile-stats-state-loading"
    | "mobile-stats-state-error"
    | "mobile-stats-state-empty"
    | null;
  offlineTestId: "mobile-stats-state-offline" | null;
  refreshFailed: boolean;
  updatedAtIct: string | null;
  summary: ChatStatisticsSummary | null;
  costDisplay: string;
  costKind: "none" | "reported" | "estimated" | "estimated_partial";
}

export function deriveChatStatisticsState(opts: {
  sessionId: string | null;
  data: ChatSessionUsage | undefined;
  isFetched: boolean;
  isError: boolean;
  error: unknown;
  dataUpdatedAt: number;
  noValueLabel?: string;
}): DerivedChatStatisticsState {
  const {
    sessionId,
    data,
    isFetched,
    isError,
    dataUpdatedAt,
    noValueLabel = "—",
  } = opts;

  // `parseWithFallback` returns `EMPTY_CHAT_SESSION_USAGE` (`session_id: ""`)
  // on schema drift or malformed response. Treat mismatch as a failed load so
  // we render `mobile-stats-state-error` with Retry and never fake zeros.
  const validData =
    data && sessionId && data.session_id === sessionId ? data : null;
  const summary = validData ? summarizeChatSessionUsage(validData) : null;
  const failed = isError || (isFetched && !validData);
  const loadFailed = failed && !summary;
  const refreshFailed = failed && !!summary;

  if (loadFailed || (!sessionId && isFetched)) {
    return {
      kind: "error",
      stateTestId: "mobile-stats-state-error",
      offlineTestId: null,
      refreshFailed: false,
      updatedAtIct: null,
      summary: null,
      costDisplay: noValueLabel,
      costKind: "none",
    };
  }

  if (!summary) {
    return {
      kind: "loading",
      stateTestId: "mobile-stats-state-loading",
      offlineTestId: null,
      refreshFailed: false,
      updatedAtIct: null,
      summary: null,
      costDisplay: noValueLabel,
      costKind: "none",
    };
  }

  const totalRuns =
    summary.runs.completed +
    summary.runs.failed +
    summary.runs.cancelled +
    summary.runs.inProgress;

  const uncostedSum = validData
    ? validData.uncosted_tokens.input +
      validData.uncosted_tokens.output +
      validData.uncosted_tokens.cache_read +
      validData.uncosted_tokens.cache_write
    : 0;
  const allReported =
    !!validData &&
    uncostedSum === 0 &&
    validData.cost_usd_ticks > 0 &&
    !summary.partial;

  const costKind: DerivedChatStatisticsState["costKind"] =
    summary.costUsd === null
      ? "none"
      : allReported
        ? "reported"
        : summary.partial
          ? "estimated_partial"
          : "estimated";

  const costDisplay =
    summary.costUsd === null ? noValueLabel : formatStatsUsd(summary.costUsd);

  const updatedAtIct =
    refreshFailed && dataUpdatedAt > 0 ? formatIctTime(dataUpdatedAt) : null;

  if (totalRuns === 0) {
    return {
      kind: "empty",
      stateTestId: "mobile-stats-state-empty",
      offlineTestId: refreshFailed ? "mobile-stats-state-offline" : null,
      refreshFailed,
      updatedAtIct,
      summary,
      costDisplay,
      costKind,
    };
  }

  return {
    kind: "populated",
    stateTestId: null,
    offlineTestId: refreshFailed ? "mobile-stats-state-offline" : null,
    refreshFailed,
    updatedAtIct,
    summary,
    costDisplay,
    costKind,
  };
}

export interface RelatedChatSessionItem {
  kind: "continued_from" | "continued_into" | "forked_from";
  id: string;
  title: string;
}

export function resolveRelatedSessions(
  session:
    | Pick<
        ChatSession,
        | "continued_from_session_id"
        | "continued_into_session_id"
        | "forked_from_session_id"
      >
    | null
    | undefined,
  sessions: readonly ChatSession[],
  fallbackTitle: string,
): RelatedChatSessionItem[] {
  if (!session) return [];
  const byId = new Map(sessions.map((s) => [s.id, s]));
  const titleOf = (id: string) => {
    const match = byId.get(id)?.title?.trim();
    return match ? match : fallbackTitle;
  };

  const out: RelatedChatSessionItem[] = [];
  if (session.continued_from_session_id) {
    out.push({
      kind: "continued_from",
      id: session.continued_from_session_id,
      title: titleOf(session.continued_from_session_id),
    });
  }
  if (session.continued_into_session_id) {
    out.push({
      kind: "continued_into",
      id: session.continued_into_session_id,
      title: titleOf(session.continued_into_session_id),
    });
  }
  if (session.forked_from_session_id) {
    out.push({
      kind: "forked_from",
      id: session.forked_from_session_id,
      title: titleOf(session.forked_from_session_id),
    });
  }
  return out;
}

export function formatStatsDuration(ms: number): string {
  if (ms <= 0) return "0s";
  const totalSec = Math.round(ms / 1000);
  if (totalSec < 60) return `${totalSec}s`;
  const min = Math.floor(totalSec / 60);
  const sec = totalSec % 60;
  if (min < 60) return sec > 0 ? `${min}m ${sec}s` : `${min}m`;
  const hr = Math.floor(min / 60);
  const remMin = min % 60;
  return remMin > 0 ? `${hr}h ${remMin}m` : `${hr}h`;
}

/**
 * Formats a timestamp in ICT (UTC+7) with the mandatory `ICT` suffix
 * (e.g. `14:32 ICT`).
 */
export function formatIctTime(timestampMs: number): string {
  const formatter = new Intl.DateTimeFormat("en-GB", {
    timeZone: "Asia/Bangkok",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
  return `${formatter.format(new Date(timestampMs))} ICT`;
}
