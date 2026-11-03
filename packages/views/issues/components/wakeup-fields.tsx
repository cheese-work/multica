"use client";

import { useId } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  Bot,
  ChevronDown,
  CircleDashed,
  CircleDot,
  Clock3,
  GitPullRequest,
  Link2,
  ListChecks,
  MessageSquare,
  RefreshCw,
  Zap,
  type IconComponent,
} from "@multica/ui/icons";
import { agentListOptions } from "@multica/core/workspace/queries";
import { Input } from "@multica/ui/components/ui/input";
import { Textarea } from "@multica/ui/components/ui/textarea";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@multica/ui/components/ui/dropdown-menu";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { AgentChoice, PillMenu, Segmented, pillClass } from "./wakeup-pickers";
import { ConditionParams } from "./wakeup-condition-params";
import {
  WAKEUP_EVENT_TYPES,
  WAKEUP_MAX_FIRES,
  WAKEUP_WAIT_DAYS,
  isEventCondition,
  wakeupConditionsFor,
  wakesAssigneeOnComments,
  type WakeupCondition,
  type WakeupDraft,
  type WakeupScope,
} from "./wakeup-draft";

/**
 * Everything a wakeup rule asks a person for: when, who to wake, what to do and
 * how long to wait. The caller owns the draft, the save and the error display.
 */
export function WakeupFields({
  draft,
  update,
  scope,
  assigneeAgentId,
  onBusy,
}: {
  draft: WakeupDraft;
  update: (patch: Partial<WakeupDraft>) => void;
  scope: WakeupScope;
  /** The issue's agent assignee, when the scope is an issue that has one. */
  assigneeAgentId: string | null;
  onBusy?: (busy: boolean) => void;
}) {
  const { t } = useT("issues");
  const id = useId();
  const { data: agents = [] } = useQuery(agentListOptions(scope.workspaceId));
  const agentName = agents.find((a) => a.id === draft.agentId)?.name;
  return (
    <>
      <div className="mt-3.5 grid grid-cols-[2.5rem_minmax(0,1fr)] items-center gap-y-2.5">
        <span className="text-label text-muted-foreground">{t(($) => $.wakeups.create.when)}</span>
        <div className="flex min-w-0 flex-wrap items-center gap-1.5">
          <ConditionMenu value={draft.condition} available={wakeupConditionsFor(scope)} onChange={(condition) => update({ condition })} />
          <ConditionParams draft={draft} scope={scope} update={update} onBusy={onBusy} />
        </div>
        <span className="text-label text-muted-foreground">{t(($) => $.wakeups.create.wake)}</span>
        <div className="flex min-w-0 items-center gap-1.5">
          <AgentChoice
            workspaceId={scope.workspaceId}
            value={draft.agentId}
            onChange={(agentId) => update({ agentId })}
            placeholder={t(($) => $.wakeups.create.choose_agent)}
          />
        </div>
      </div>
      {wakesAssigneeOnComments(draft, assigneeAgentId) && (
        <p className="mt-2.5 rounded-md bg-muted/60 px-2.5 py-2 text-caption leading-5 text-muted-foreground">
          {t(($) => $.wakeups.create.assignee_comment_hint, { name: agentName ?? "" })}
        </p>
      )}
      {draft.condition === "at" && draft.atPreset === "custom" && (
        <Input
          type="datetime-local"
          aria-label={t(($) => $.wakeups.local_time, { timezone: Intl.DateTimeFormat().resolvedOptions().timeZone })}
          className="mt-2.5"
          value={draft.atCustom}
          onChange={(event) => update({ atCustom: event.target.value })}
        />
      )}
      <label htmlFor={`${id}-instruction`} className="mt-4 mb-1.5 block text-caption font-medium">
        {t(($) => $.wakeups.instruction_title)}
      </label>
      <Textarea
        id={`${id}-instruction`}
        rows={3}
        value={draft.instruction}
        placeholder={t(($) => $.wakeups.create.instruction_placeholder)}
        className="max-h-[30dvh] resize-y text-base md:text-label"
        onChange={(event) => update({ instruction: event.target.value })}
      />
      {draft.condition === "recurring" && (
        <div className="mt-4 grid grid-cols-[4.5rem_minmax(0,1fr)] items-center gap-y-2.5">
          <label htmlFor={`${id}-until`} className="text-caption text-muted-foreground">
            {t(($) => $.wakeups.create.until)}
          </label>
          <Input
            id={`${id}-until`}
            type="date"
            className="h-7 w-44"
            value={draft.until}
            onChange={(event) => update({ until: event.target.value })}
          />
        </div>
      )}
      {isEventCondition(draft.condition) && (
        <div className="mt-4 grid grid-cols-[4.5rem_minmax(0,1fr)] items-center gap-y-2.5">
          <span className="text-caption text-muted-foreground">{t(($) => $.wakeups.create.trigger_count)}</span>
          <Segmented
            label={t(($) => $.wakeups.create.trigger_count)}
            value={draft.mode}
            options={[
              { value: "once", label: t(($) => $.wakeups.once) },
              { value: "continuous", label: t(($) => $.wakeups.continuous) },
            ]}
            onChange={(mode) => update({ mode })}
          />
          {draft.mode === "continuous" && (
            <>
              <span className="text-caption text-muted-foreground">{t(($) => $.wakeups.create.max_fires)}</span>
              <div className="flex min-w-0">
                <PillMenu
                  label={t(($) => $.wakeups.create.max_fires)}
                  value={String(draft.maxFires)}
                  options={WAKEUP_MAX_FIRES.map((count) => ({
                    value: String(count),
                    label: t(($) => $.wakeups.create.max_fires_times, { count }),
                  }))}
                  onChange={(value) => update({ maxFires: Number(value) })}
                />
              </div>
            </>
          )}
          <span className="text-caption text-muted-foreground">{t(($) => $.wakeups.create.max_wait)}</span>
          <div className="flex min-w-0">
            <PillMenu
              label={t(($) => $.wakeups.create.max_wait)}
              value={String(draft.waitDays)}
              options={WAKEUP_WAIT_DAYS.map((days) => ({
                value: String(days),
                label: t(($) => $.wakeups.create.wait_days, { count: days }),
              }))}
              onChange={(value) => update({ waitDays: Number(value) })}
            />
          </div>
          {!scope.definition && (
            <>
              <span className="text-caption text-muted-foreground">{t(($) => $.wakeups.create.on_timeout)}</span>
              <div className="flex min-w-0">
                <PillMenu
                  label={t(($) => $.wakeups.create.on_timeout)}
                  value={draft.onTimeout}
                  options={[
                    {
                      value: "wake",
                      label: t(($) => $.wakeups.create.timeout_wake, {
                        agent: agentName ?? t(($) => $.wakeups.create.the_agent),
                      }),
                    },
                    { value: "end", label: t(($) => $.wakeups.create.timeout_end) },
                  ]}
                  onChange={(value) => update({ onTimeout: value as WakeupDraft["onTimeout"] })}
                />
              </div>
            </>
          )}
        </div>
      )}
    </>
  );
}

function ConditionMenu({
  value,
  available,
  onChange,
}: {
  value: WakeupCondition | null;
  available: WakeupCondition[];
  onChange: (value: WakeupCondition) => void;
}) {
  const { t } = useT("issues");
  const items: Record<WakeupCondition, { icon: IconComponent; label: string; hint: string }> = {
    at: { icon: Clock3, label: t(($) => $.wakeups.create.cond_at), hint: t(($) => $.wakeups.create.cond_at_hint) },
    recurring: { icon: RefreshCw, label: t(($) => $.wakeups.create.cond_recurring), hint: t(($) => $.wakeups.create.cond_recurring_hint) },
    reply: { icon: MessageSquare, label: t(($) => $.wakeups.create.cond_reply), hint: t(($) => $.wakeups.create.cond_reply_hint) },
    field: { icon: CircleDot, label: t(($) => $.wakeups.create.cond_field), hint: t(($) => $.wakeups.create.cond_field_hint) },
    run_end: { icon: Bot, label: t(($) => $.wakeups.create.cond_run_end), hint: t(($) => $.wakeups.create.cond_run_end_hint) },
    children: { icon: ListChecks, label: t(($) => $.wakeups.create.cond_children), hint: t(($) => $.wakeups.create.cond_children_hint) },
    pull_request: { icon: GitPullRequest, label: t(($) => $.wakeups.create.cond_pr), hint: t(($) => $.wakeups.create.cond_pr_hint) },
    other_issue: { icon: Link2, label: t(($) => $.wakeups.create.cond_issue), hint: t(($) => $.wakeups.create.cond_issue_hint) },
    custom: { icon: Zap, label: t(($) => $.wakeups.create.cond_custom), hint: t(($) => $.wakeups.create.cond_custom_hint, { count: WAKEUP_EVENT_TYPES.length }) },
  };
  const groups = (
    [
      [t(($) => $.wakeups.create.group_time), ["at", "recurring"]],
      [t(($) => $.wakeups.create.group_collaboration), ["reply", "field"]],
      [t(($) => $.wakeups.create.group_runs), ["run_end", "children"]],
      [t(($) => $.wakeups.create.group_linked), ["pull_request", "other_issue"]],
    ] as [string, WakeupCondition[]][]
  )
    .map(([label, keys]): [string, WakeupCondition[]] => [label, keys.filter((key) => available.includes(key))])
    .filter(([, keys]) => keys.length > 0);
  const selected = value ? items[value] : null;
  const SelectedIcon = selected?.icon ?? CircleDashed;
  const option = (key: WakeupCondition) => {
    const Icon = items[key].icon;
    return (
      <DropdownMenuItem key={key} onClick={() => onChange(key)} className="items-start gap-2 py-1.5">
        <span className="flex h-5 shrink-0 items-center">
          <Icon className="size-3.5 text-muted-foreground" aria-hidden="true" />
        </span>
        <span className="flex min-w-0 flex-col gap-0.5">
          <span>{items[key].label}</span>
          <span className="text-caption text-muted-foreground">{items[key].hint}</span>
        </span>
      </DropdownMenuItem>
    );
  };
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={<button type="button" className={cn(pillClass, !value && "border-ring")} />}
      >
        <SelectedIcon className="size-3.5 text-muted-foreground" aria-hidden="true" />
        <span className={cn("truncate", !selected && "text-muted-foreground")}>
          {selected?.label ?? t(($) => $.wakeups.create.choose_condition)}
        </span>
        <ChevronDown className="size-3 text-muted-foreground" aria-hidden="true" />
      </DropdownMenuTrigger>
      <DropdownMenuContent className="w-72">
        {groups.map(([label, keys]) => (
          <DropdownMenuGroup key={label}>
            <DropdownMenuLabel>{label}</DropdownMenuLabel>
            {keys.map(option)}
          </DropdownMenuGroup>
        ))}
        {groups.length > 0 && <DropdownMenuSeparator />}
        {available.includes("custom") && option("custom")}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
