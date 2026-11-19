/**
 * Inbox title display helpers.
 *
 * Mirrors packages/views/inbox/components/inbox-display.ts. Keeping behavior
 * identical is required by apps/mobile/CLAUDE.md "Behavioral parity":
 * the title a user sees in the mobile inbox MUST match what they see on
 * web for the same item. When the web version changes, sync this file.
 */
import type { InboxItem, TimelineEntry } from "@multica/core/types";
import { groupInboxItemsByIssue } from "@multica/core/inbox/group";
import { i18n } from "@/lib/i18n/singleton";

function formatResetAt(value: string | undefined): string {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return new Intl.DateTimeFormat(i18n.resolvedLanguage ?? i18n.language, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(date);
}

export function getAutopilotQuotaBody(item: InboxItem): string | null {
  if (item.type !== "autopilot_quota_exceeded") return item.body;
  const details = item.details ?? {};
  const resetAt = formatResetAt(details.reset_at);
  if (!details.limit || !resetAt) return item.body;
  const t = i18n.t.bind(i18n);
  if (details.autopilot_title) {
    return t("inbox:body.autopilot_title_limit", {
      title: details.autopilot_title,
      limit: details.limit,
      resetAt,
    });
  }
  return t("inbox:body.autopilot_limit", {
    limit: details.limit,
    resetAt,
  });
}

/**
 * Body of an issue-less notice, in the UI locale where the client has the
 * facts to write it and as the server sent it otherwise.
 */
export function getInboxNoticeBody(item: InboxItem): string | null {
  if (item.type === "project_owner_joined" && item.details?.joined_by) {
    return i18n.t("inbox:body.project_owner_joined", {
      name: item.details.joined_by,
    });
  }
  return getAutopilotQuotaBody(item);
}

function singleLine(value: string | null | undefined): string {
  return (value ?? "").replace(/\s+/g, " ").trim();
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

export function stripQuickCreatePrefix(
  title: string,
  identifier?: string,
): string {
  const normalized = singleLine(title);
  if (!normalized) return "";
  if (identifier) {
    const exactPrefix = new RegExp(
      `^Created\\s+${escapeRegExp(identifier)}:\\s*`,
      "i",
    );
    const withoutExactPrefix = normalized.replace(exactPrefix, "");
    if (withoutExactPrefix !== normalized) return withoutExactPrefix.trim();
  }
  return normalized.replace(/^Created\s+[A-Z][A-Z0-9]*-\d+:\s*/i, "").trim();
}

export function getInboxDisplayTitle(item: InboxItem): string {
  const details = item.details ?? {};
  // Resolve system-notice titles through the selected UI locale. Mirror
  // rather than exposing backend fallback copy that can include raw counts.
  switch (item.type) {
    case "autopilot_quota_exceeded":
      return i18n.t("inbox:type.autopilot_quota_exceeded");
  }
  if (item.type === "quick_create_done") {
    const cleanedTitle = stripQuickCreatePrefix(item.title, details.identifier);
    if (cleanedTitle) return cleanedTitle;
    const prompt = singleLine(details.original_prompt);
    if (prompt) return prompt;
  }
  // Both non-success quick-create outcomes surface the user's original input
  // as the row title. Mirrors isQuickCreateOutcome in
  // packages/views/inbox/components/inbox-display.ts.
  if (item.type === "quick_create_failed" || item.type === "quick_create_unconfirmed") {
    const prompt = singleLine(details.original_prompt);
    if (prompt) return prompt;
  }
  return item.title;
}

/** Issue-less notices that open in the inbox's own detail sheet. */
export function isInboxNotice(type: InboxItem["type"]): boolean {
  return (
    type === "autopilot_quota_exceeded" ||
    type === "autopilot_paused" ||
    type === "project_owner_joined"
  );
}

export function getInboxNavigationTarget(
  item: InboxItem,
  workspace: string | null,
  historyToken: string,
) {
  if (!workspace) return null;
  if (item.issue_id) {
    return {
      pathname: "/[workspace]/issue/[id]" as const,
      params: {
        workspace,
        id: item.issue_id,
        highlight: item.details?.comment_id,
        h: historyToken,
      },
    };
  }
  if (isInboxNotice(item.type)) {
    return {
      pathname: "/[workspace]/inbox/[id]" as const,
      params: { workspace, id: item.id },
    };
  }
  return null;
}

/**
 * True when the viewer was named directly or by `@all`. Mirrors
 * packages/core/inbox/filter-store.ts isViewerMention: the server writes
 * `mentioned` for `@squad` too, with `details.mention_source` telling them
 * apart; a row without it has an unknown origin and does not match.
 */
export function isViewerMention(item: InboxItem): boolean {
  const source = item.details?.mention_source;
  return source === "direct" || source === "all";
}

/**
 * The timeline row a deep-link `highlight` lands on. A failed run's
 * notification names its task; mobile renders no run rows, so the run shows
 * only as the failure notice it posted, and the link lands there. Null
 * while the timeline has no such row — a cached timeline may not have it
 * yet, and a run that posted nothing never will.
 */
export function resolveTimelineHighlight(
  entries: readonly TimelineEntry[],
  highlight: string,
): string | null {
  if (entries.some((entry) => entry.id === highlight)) return highlight;
  const posted = entries.filter(
    (entry) => entry.type === "comment" && entry.source_task_id === highlight,
  );
  return (
    posted.find((entry) => entry.comment_type === "system") ?? posted.at(-1)
  )?.id ?? null;
}

/**
 * The inbox list: archived rows dropped, one row per issue, each opening at
 * the timeline entry web opens it at. The grouping and anchor rule is core's
 * `groupInboxItemsByIssue`, the one web runs. Mobile renders no app
 * requests, so an app request row anchors like any other row instead of
 * keeping its request.
 *
 * Mirrors packages/core/inbox/queries.ts deduplicateInboxItems. **MUST stay
 * aligned with that function** — see the inbox dedup incident in this file's
 * companion `apps/mobile/CLAUDE.md` "Behavioral parity" section. Skipping
 * this step makes the same workspace/user show different unread counts on
 * mobile vs web.
 *
 * A grouped viewer mention's source is copied onto the kept row, so a later
 * comment does not hide it from the "Mentions me" filter.
 */
export function deduplicateInboxItems(items: InboxItem[]): InboxItem[] {
  const active = items.filter((i) => !i.archived);
  const mentions = new Map<string, InboxItem>();
  for (const item of active) {
    if (!isViewerMention(item)) continue;
    const key = item.issue_id ?? item.id;
    const seen = mentions.get(key);
    if (!seen || new Date(item.created_at).getTime() > new Date(seen.created_at).getTime()) {
      mentions.set(key, item);
    }
  }
  return groupInboxItemsByIssue(
    active,
    { keepRequestTarget: false },
  ).map((row) => {
    // A later comment must not hide a mention from the "Mentions me" filter.
    const mention = isViewerMention(row) ? undefined : mentions.get(row.issue_id ?? row.id);
    const source = mention?.details?.mention_source;
    return source === undefined
      ? row
      : { ...row, details: { ...(row.details ?? {}), mention_source: source } };
  });
}
