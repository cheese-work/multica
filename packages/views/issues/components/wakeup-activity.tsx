"use client";

import { Bell, BellOff, Clock3, Hourglass, ListChecks, RefreshCw, TriangleAlert } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { issueTasksOptions } from "@multica/core/issues/queries";
import { useWorkspacePaths } from "@multica/core/paths";
import type { TimelineEntry, WakeupCondition, WakeupPreview } from "@multica/core/types";
import { useT } from "../../i18n";
import { AppLink } from "../../navigation";
import { TranscriptButton } from "../../common/task-transcript";
import type { useWakeupText } from "./wakeup-presentation";
import { conditionIcon } from "./wakeups-section";

type IssuesT = ReturnType<typeof useT<"issues">>["t"];
type WakeupText = ReturnType<typeof useWakeupText>;
type ActorName = (type: string, id: string) => string;

/** Timeline entries the wakeup service and the child-done rule write. */
export const WAKEUP_ACTIVITY_ACTIONS = new Set([
  "wakeup_created",
  "wakeup_triggered",
  "wakeup_skipped",
  "wakeup_timed_out",
  "wakeup_paused",
  "wakeup_checkin",
]);

interface StoredPreview {
  id?: string;
  kind?: WakeupPreview["kind"];
  mode?: WakeupPreview["mode"];
  event_types?: string[];
  agent_id?: string;
  timezone?: string;
  condition?: WakeupCondition;
  filter_actor_type?: "member" | "agent";
  filter_actor_id?: string;
  filter_agent_id?: string;
  interval_seconds?: number;
  cron_expression?: string;
  next_fire_at?: string;
  created_by?: string;
  created_by_agent_id?: string;
}

interface WakeupDetails {
  wakeup?: StoredPreview;
  /** The built-in rule that fired; user-made rules carry none. */
  rule?: "child_done" | "pr_merged" | "pr_checks_failed" | "pr_needs_attention";
  pr_number?: number;
  pr_url?: string;
  comment_id?: string;
  task_id?: string;
  stage?: number;
  total?: number;
  target_type?: string;
  target_id?: string;
  /**
   * woke (a run), notified (a member), merged (joined a run already waiting
   * to start), acknowledged (the agent's own action; no run), none.
   */
  outcome?: "woke" | "notified" | "merged" | "acknowledged" | "none";
  events?: string[];
  actor_type?: string;
  actor_id?: string;
  woke?: boolean;
  /** Why it paused ("max_fires", "loop", "rate") or, on a skip, the sentence. */
  reason?: string;
  reason_code?: string;
  limit?: number;
  note?: string;
}

const PR_RULES = new Set(["pr_merged", "pr_checks_failed", "pr_needs_attention"]);

function detailsOf(entry: TimelineEntry): WakeupDetails {
  return (entry.details ?? {}) as WakeupDetails;
}

/**
 * The rule as it was when the entry was written, in the shape the wakeup text
 * helpers read. Names come from the workspace directory.
 */
function previewOf(stored: StoredPreview, getActorName: ActorName): WakeupPreview {
  return {
    id: stored.id ?? "",
    issue_id: "",
    agent_id: stored.agent_id ?? "",
    agent_name: stored.agent_id ? getActorName("agent", stored.agent_id) : "",
    kind: stored.kind ?? "event",
    mode: stored.mode ?? "once",
    event_types: stored.event_types ?? [],
    filter_task_id: null,
    filter_agent_name: stored.filter_agent_id ? getActorName("agent", stored.filter_agent_id) : null,
    filter_actor_type: stored.filter_actor_type ?? null,
    filter_actor_id: stored.filter_actor_id ?? null,
    filter_actor_name: stored.filter_actor_type && stored.filter_actor_id ? getActorName(stored.filter_actor_type, stored.filter_actor_id) : null,
    interval_seconds: stored.interval_seconds ?? null,
    cron_expression: stored.cron_expression ?? null,
    timezone: stored.timezone ?? "UTC",
    next_fire_at: stored.next_fire_at ?? null,
    condition: stored.condition ?? null,
  };
}

/**
 * What fired, from what the entry recorded: the built-in PR rule with its PR,
 * else the event and who caused it, else the rule's own condition. Never an
 * empty event name.
 */
function triggerOf(details: WakeupDetails, preview: WakeupPreview | null, t: IssuesT, text: WakeupText, getActorName: ActorName): string {
  if (details.rule && PR_RULES.has(details.rule)) {
    const pr = details.pr_number ? `PR #${details.pr_number}` : "PR";
    switch (details.rule) {
      case "pr_merged":
        return t(($) => $.activity.trigger_pr_merged, { pr });
      case "pr_checks_failed":
        return t(($) => $.activity.trigger_pr_checks_failed, { pr });
      default:
        return t(($) => $.activity.trigger_pr_needs_attention, { pr });
    }
  }
  const event = details.events?.[0];
  const name = details.actor_type && details.actor_id ? getActorName(details.actor_type, details.actor_id) : "";
  if (event === "comment.created") {
    return name ? t(($) => $.activity.trigger_comment_by, { name }) : t(($) => $.activity.trigger_comment);
  }
  if (event === "issue.status_changed") {
    return name ? t(($) => $.activity.trigger_status_by, { name }) : t(($) => $.activity.trigger_status);
  }
  // "condition.met" is the scheduler's own event for a condition rule, not
  // something that happened on the issue: read it as the rule's condition.
  if (event && event !== "condition.met") return text.eventName(event);
  const named = preview && (preview.condition || preview.kind !== "event" || preview.event_types[0]);
  return named ? text.trigger(preview) : t(($) => $.activity.trigger_unknown);
}

export function formatWakeupActivity(
  entry: TimelineEntry,
  t: IssuesT,
  text: WakeupText,
  getActorName: ActorName,
): string {
  const details = detailsOf(entry);
  const preview = details.wakeup ? previewOf(details.wakeup, getActorName) : null;
  const condition = preview ? text.trigger(preview) : "";
  const agent = preview?.agent_name ?? "";
  // A built-in rule's preview names no agent; the entry names who was woken.
  const woken =
    (details.target_type && details.target_id ? getActorName(details.target_type, details.target_id) : agent) ||
    t(($) => $.activity.wakeup_assignee);
  switch (entry.action) {
    case "wakeup_created":
      // A time rule's label already says "wake at ...", so the sentence names
      // the agent up front instead of repeating "wake" after it.
      return preview && !preview.condition && preview.kind !== "event"
        ? t(($) => $.activity.wakeup_created_scheduled, { condition, agent })
        : t(($) => $.activity.wakeup_created, { condition, agent });
    case "wakeup_triggered": {
      if (details.rule === "child_done") {
        const count = details.total ?? 1;
        const closed = details.stage
          ? t(($) => $.activity.wakeup_child_done_stage, { stage: details.stage, count })
          : t(($) => $.activity.wakeup_child_done_all, { count });
        // The squad's leader ran, but the entry names the assignee people see.
        const name =
          details.target_type && details.target_id
            ? getActorName(details.target_type, details.target_id)
            : t(($) => $.activity.wakeup_assignee);
        const outcome = details.outcome ?? (details.target_type ? "woke" : "none");
        switch (outcome) {
          case "woke":
            return closed + t(($) => $.activity.wakeup_child_done_woke, { name });
          case "notified":
            return closed + t(($) => $.activity.wakeup_child_done_notified, { name });
          case "merged":
            return closed + t(($) => $.activity.wakeup_child_done_merged, { name });
          case "acknowledged":
            return closed + t(($) => $.activity.wakeup_child_done_acknowledged, { name });
          default:
            return closed;
        }
      }
      if (details.events?.length === 1 && details.events[0] === "wakeup.manual" && details.actor_id) {
        return t(($) => $.activity.wakeup_triggered_manual, { name: getActorName("member", details.actor_id), agent });
      }
      const fired = triggerOf(details, preview, t, text, getActorName);
      if (details.outcome === "merged") return t(($) => $.activity.wakeup_triggered_merged, { condition: fired, agent: woken });
      if (details.outcome === "acknowledged") return t(($) => $.activity.wakeup_triggered_acknowledged, { condition: fired, agent: woken });
      return t(($) => $.activity.wakeup_triggered, { condition: fired, agent: woken });
    }
    case "wakeup_skipped": {
      const fired = triggerOf(details, preview, t, text, getActorName);
      const reason = details.reason ?? details.reason_code;
      return reason
        ? t(($) => $.activity.wakeup_skipped, { condition: fired, agent: woken, reason })
        : t(($) => $.activity.wakeup_skipped_plain, { condition: fired, agent: woken });
    }
    case "wakeup_timed_out":
      return details.woke
        ? t(($) => $.activity.wakeup_timed_out_wake, { condition, agent })
        : t(($) => $.activity.wakeup_timed_out_end, { condition });
    case "wakeup_paused":
      return text.paused({ paused_reason: (details.reason as "max_fires" | "loop" | "rate" | undefined) ?? "rate", max_fires: details.limit ?? null, fire_count: details.limit ?? 0 }) ?? "";
    case "wakeup_checkin": {
      const count = entry.coalesced_count ?? 1;
      const note = details.note ?? "";
      return count > 1
        ? t(($) => $.activity.wakeup_checkin_coalesced, { count, note })
        : t(($) => $.activity.wakeup_checkin, { note });
    }
    default:
      return entry.action ?? "";
  }
}

/**
 * The small tag on the right of an entry that names the rule it came from:
 * the system rule, a schedule for check-ins, or who created the rule.
 */
export function wakeupActivityChip(
  entry: TimelineEntry,
  t: IssuesT,
  text: WakeupText,
  getActorName: ActorName,
): string | null {
  const details = detailsOf(entry);
  if (details.rule) return t(($) => $.activity.wakeup_chip_system);
  if (entry.action === "wakeup_created" || !details.wakeup) return null;
  if (entry.action === "wakeup_checkin") return text.trigger(previewOf(details.wakeup, getActorName));
  const creator = details.wakeup.created_by_agent_id
    ? getActorName("agent", details.wakeup.created_by_agent_id)
    : details.wakeup.created_by
      ? getActorName("member", details.wakeup.created_by)
      : null;
  return creator ? t(($) => $.activity.wakeup_chip_created_by, { name: creator }) : null;
}

export function WakeupActivityIcon({ entry }: { entry: TimelineEntry }) {
  const details = detailsOf(entry);
  const className = "h-4 w-4 shrink-0 text-muted-foreground";
  if (details.rule === "child_done") return <ListChecks className={className} aria-hidden="true" />;
  switch (entry.action) {
    case "wakeup_timed_out":
      return <Hourglass className={className} aria-hidden="true" />;
    case "wakeup_paused":
      return <TriangleAlert className="h-4 w-4 shrink-0 text-warning" aria-hidden="true" />;
    case "wakeup_checkin":
      return <RefreshCw className={className} aria-hidden="true" />;
    case "wakeup_skipped":
      return <BellOff className={className} aria-hidden="true" />;
  }
  const Icon = conditionIcon(details.wakeup?.condition) ?? (details.wakeup?.kind && details.wakeup.kind !== "event" ? Clock3 : Bell);
  return <Icon className={className} aria-hidden="true" />;
}

export interface WakeupActivityLinkTargets {
  pr?: { number: number; url: string };
  commentId?: string;
  taskId?: string;
}

/** The PR, comment and run an entry recorded; only what is present and safe to open. */
export function wakeupActivityLinks(entry: TimelineEntry): WakeupActivityLinkTargets {
  const { pr_number, pr_url, comment_id, task_id } = detailsOf(entry);
  const links: WakeupActivityLinkTargets = {};
  if (pr_number && pr_url && /^https:\/\//.test(pr_url)) links.pr = { number: pr_number, url: pr_url };
  if (comment_id) links.commentId = comment_id;
  if (task_id) links.taskId = task_id;
  return links;
}

/** Links on a wakeup row: the PR, the triggering comment and the started run. */
export function WakeupActivityLinks({ entry, issueId, getActorName }: { entry: TimelineEntry; issueId: string; getActorName: ActorName }) {
  const { t } = useT("issues");
  const paths = useWorkspacePaths();
  const { data: tasks = [] } = useQuery(issueTasksOptions(issueId));
  const { pr, commentId, taskId } = wakeupActivityLinks(entry);
  const task = taskId ? tasks.find((candidate) => candidate.id === taskId) : undefined;
  if (!pr && !commentId && !task) return null;
  const link = "shrink-0 font-medium text-foreground underline-offset-4 hover:underline";
  return (
    <>
      {pr && (
        <a href={pr.url} target="_blank" rel="noopener noreferrer" className={link} data-testid="wakeup-activity-pr-link">
          {t(($) => $.activity.wakeup_link_pr, { number: pr.number })}
        </a>
      )}
      {commentId && (
        <AppLink href={`${paths.issueDetail(issueId)}#comment-${commentId}`} className={link} data-testid="wakeup-activity-comment-link">
          {t(($) => $.activity.wakeup_link_comment)}
        </AppLink>
      )}
      {task && (
        <span className="contents" data-testid="wakeup-activity-run-link">
          <TranscriptButton task={task} agentName={getActorName("agent", task.agent_id)} title={t(($) => $.activity.wakeup_link_run)} />
        </span>
      )}
    </>
  );
}
