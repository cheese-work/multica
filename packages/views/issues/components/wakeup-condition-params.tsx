"use client";

import { useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { ChevronDown, Link2 } from "@multica/ui/icons";
import { childIssuesOptions } from "@multica/core/issues";
import { useIssueStatuses } from "@multica/core/issue-statuses/hooks";
import { labelListOptions } from "@multica/core/labels/queries";
import { propertyListOptions } from "@multica/core/properties/queries";
import { Input } from "@multica/ui/components/ui/input";
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuTrigger,
} from "@multica/ui/components/ui/dropdown-menu";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { IssuePickerModal } from "../../modals/issue-picker-modal";
import { useStatusLabel } from "../utils/status-label";
import { useWakeupText } from "./wakeup-presentation";
import { ActorChoice, AgentChoice, AssigneeChoice, PillMenu, pillClass } from "./wakeup-pickers";
import {
  WAKEUP_EVENT_TYPES,
  type WakeupAtPreset,
  type WakeupDraft,
  type WakeupField,
  type WakeupRecurrence,
  type WakeupScope,
} from "./wakeup-draft";

export function ConditionParams({
  draft,
  scope,
  update,
  onBusy,
}: {
  draft: WakeupDraft;
  scope: WakeupScope;
  update: (patch: Partial<WakeupDraft>) => void;
  onBusy?: (busy: boolean) => void;
}) {
  const { t } = useT("issues");
  const text = useWakeupText();
  const { workspaceId, issueId } = scope;
  switch (draft.condition) {
    case "field":
      return <FieldParams draft={draft} workspaceId={workspaceId} update={update} />;
    case "children":
      return <StageChoice workspaceId={workspaceId} issueId={issueId} value={draft.stage} onChange={(stage) => update({ stage })} />;
    case "pull_request":
      return (
        <PillMenu
          label={t(($) => $.wakeups.create.cond_pr)}
          value={draft.prEvent}
          options={[
            { value: "checks_finished", label: t(($) => $.wakeups.create.pr_checks) },
            { value: "merged", label: t(($) => $.wakeups.create.pr_merged) },
            { value: "conflicts", label: t(($) => $.wakeups.create.pr_conflicts) },
          ]}
          onChange={(prEvent) => update({ prEvent: prEvent as WakeupDraft["prEvent"] })}
        />
      );
    case "other_issue":
      return (
        <>
          <OtherIssueChoice issueId={issueId} value={draft.otherIssue} onChange={(otherIssue) => update({ otherIssue })} onBusy={onBusy} />
          <PillMenu
            label={t(($) => $.wakeups.create.cond_issue)}
            value={draft.otherState}
            options={[
              { value: "done", label: t(($) => $.wakeups.create.other_done) },
              { value: "ended", label: t(($) => $.wakeups.create.other_ended) },
              { value: "in_review", label: t(($) => $.wakeups.create.other_in_review) },
              { value: "blocked", label: t(($) => $.wakeups.create.other_blocked) },
            ]}
            onChange={(otherState) => update({ otherState: otherState as WakeupDraft["otherState"] })}
          />
        </>
      );
    case "at":
      return (
        <PillMenu
          label={t(($) => $.wakeups.create.cond_at)}
          value={draft.atPreset}
          options={[
            { value: "10m", label: t(($) => $.wakeups.create.at_10m) },
            { value: "1h", label: t(($) => $.wakeups.create.at_1h) },
            { value: "tomorrow", label: t(($) => $.wakeups.create.at_tomorrow) },
            { value: "custom", label: t(($) => $.wakeups.create.at_custom) },
          ]}
          onChange={(atPreset) => update({ atPreset: atPreset as WakeupAtPreset })}
        />
      );
    case "recurring":
      return (
        <PillMenu
          label={t(($) => $.wakeups.create.cond_recurring)}
          value={draft.recurrence}
          options={[
            { value: "hourly", label: t(($) => $.wakeups.create.hourly) },
            { value: "daily", label: t(($) => $.wakeups.create.daily) },
            { value: "weekdays", label: t(($) => $.wakeups.create.weekdays) },
          ]}
          onChange={(recurrence) => update({ recurrence: recurrence as WakeupRecurrence })}
        />
      );
    case "reply":
      return <ActorChoice workspaceId={workspaceId} value={draft.replyActor} onChange={(replyActor) => update({ replyActor })} />;
    case "run_end":
      return (
        <AgentChoice
          workspaceId={workspaceId}
          value={draft.runAgentId}
          onChange={(runAgentId) => update({ runAgentId })}
          placeholder={t(($) => $.wakeups.create.any_agent)}
          allowAny
        />
      );
    case "custom":
      return (
        <DropdownMenu>
          <DropdownMenuTrigger render={<button type="button" className={pillClass} />}>
            <span className={cn(draft.events.length === 0 && "text-muted-foreground")}>
              {t(($) => $.wakeups.create.events_selected, { count: draft.events.length })}
            </span>
            <ChevronDown className="size-3 text-muted-foreground" aria-hidden="true" />
          </DropdownMenuTrigger>
          <DropdownMenuContent className="max-h-80 w-80">
            {WAKEUP_EVENT_TYPES.map((event) => (
              <DropdownMenuCheckboxItem
                key={event}
                checked={draft.events.includes(event)}
                closeOnClick={false}
                onCheckedChange={(checked) =>
                  update({
                    events: checked
                      ? WAKEUP_EVENT_TYPES.filter((e) => e === event || draft.events.includes(e))
                      : draft.events.filter((e) => e !== event),
                  })
                }
              >
                {text.eventName(event)}
              </DropdownMenuCheckboxItem>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>
      );
    default:
      return null;
  }
}

/** Field, then the value it has to reach. */
function FieldParams({
  draft,
  workspaceId,
  update,
}: {
  draft: WakeupDraft;
  workspaceId: string;
  update: (patch: Partial<WakeupDraft>) => void;
}) {
  const { t } = useT("issues");
  const statuses = useIssueStatuses(workspaceId);
  const statusLabel = useStatusLabel(workspaceId);
  const { data: labels = [] } = useQuery(labelListOptions(workspaceId));
  const { data: allProperties = [] } = useQuery(propertyListOptions(workspaceId));
  const properties = allProperties.filter((p) => !p.archived);
  const property = properties.find((p) => p.id === draft.fieldTarget);
  const choose = (field: WakeupField) => update({ field, fieldTarget: "", fieldValue: "", assignee: null });
  const fieldMenu = (
    <PillMenu
      label={t(($) => $.wakeups.create.field_kind)}
      value={draft.field}
      options={[
        { value: "status", label: t(($) => $.wakeups.create.field_status) },
        { value: "assignee", label: t(($) => $.wakeups.create.field_assignee) },
        { value: "label", label: t(($) => $.wakeups.create.field_label) },
        { value: "property", label: t(($) => $.wakeups.create.field_property) },
      ]}
      onChange={(field) => choose(field as WakeupField)}
    />
  );
  let value: ReactNode = null;
  if (draft.field === "status") {
    value = (
      <PillMenu
        label={t(($) => $.wakeups.create.choose_status)}
        placeholder={t(($) => $.wakeups.create.choose_status)}
        value={draft.fieldTarget}
        options={statuses.activeStatuses.map((s) => ({ value: s.key, label: statusLabel(s.key) }))}
        onChange={(fieldTarget) => update({ fieldTarget })}
      />
    );
  } else if (draft.field === "assignee") {
    value = <AssigneeChoice workspaceId={workspaceId} value={draft.assignee} onChange={(assignee) => update({ assignee })} />;
  } else if (draft.field === "label") {
    value = (
      <PillMenu
        label={t(($) => $.wakeups.create.choose_label)}
        placeholder={t(($) => $.wakeups.create.choose_label)}
        value={draft.fieldTarget}
        options={labels.map((l) => ({ value: l.id, label: l.name }))}
        onChange={(fieldTarget) => update({ fieldTarget })}
      />
    );
  } else {
    value = (
      <>
        <PillMenu
          label={t(($) => $.wakeups.create.choose_property)}
          placeholder={t(($) => $.wakeups.create.choose_property)}
          value={draft.fieldTarget}
          options={properties.map((p) => ({ value: p.id, label: p.name }))}
          onChange={(fieldTarget) => update({ fieldTarget, fieldValue: "" })}
        />
        {property &&
          (property.type === "select" || property.type === "multi_select" ? (
            <PillMenu
              label={property.name}
              placeholder={t(($) => $.wakeups.create.value_placeholder)}
              value={draft.fieldValue}
              options={(property.config.options ?? []).map((o) => ({ value: o.id, label: o.name }))}
              onChange={(fieldValue) => update({ fieldValue })}
            />
          ) : property.type === "checkbox" ? (
            <PillMenu
              label={property.name}
              placeholder={t(($) => $.wakeups.create.value_placeholder)}
              value={draft.fieldValue}
              options={[
                { value: "true", label: t(($) => $.wakeups.create.checked) },
                { value: "false", label: t(($) => $.wakeups.create.unchecked) },
              ]}
              onChange={(fieldValue) => update({ fieldValue })}
            />
          ) : (
            <Input
              aria-label={property.name}
              placeholder={t(($) => $.wakeups.create.value_placeholder)}
              type={property.type === "number" ? "number" : "text"}
              className="h-7 w-32 text-base md:text-label"
              value={draft.fieldValue}
              onChange={(event) => update({ fieldValue: event.target.value })}
            />
          ))}
      </>
    );
  }
  return (
    <>
      {fieldMenu}
      {value}
    </>
  );
}

/** Every sub-issue, or one of the stages this issue's sub-issues use. */
function StageChoice({
  workspaceId,
  issueId,
  value,
  onChange,
}: {
  workspaceId: string;
  issueId?: string;
  value: number | null;
  onChange: (stage: number | null) => void;
}) {
  const { t } = useT("issues");
  const { data: children = [] } = useQuery({ ...childIssuesOptions(workspaceId, issueId ?? ""), enabled: !!issueId });
  const stages = [...new Set(children.map((c) => c.stage).filter((s): s is number => typeof s === "number"))].sort((a, b) => a - b);
  return (
    <PillMenu
      label={t(($) => $.wakeups.create.cond_children)}
      value={value === null ? "all" : String(value)}
      options={[
        { value: "all", label: t(($) => $.wakeups.create.children_all) },
        ...stages.map((stage) => ({ value: String(stage), label: t(($) => $.wakeups.create.children_stage, { stage }) })),
      ]}
      onChange={(next) => onChange(next === "all" ? null : Number(next))}
    />
  );
}

function OtherIssueChoice({
  issueId,
  value,
  onChange,
  onBusy,
}: {
  issueId?: string;
  value: WakeupDraft["otherIssue"];
  onChange: (value: WakeupDraft["otherIssue"]) => void;
  onBusy?: (busy: boolean) => void;
}) {
  const { t } = useT("issues");
  const [open, setOpen] = useState(false);
  // The picker is a dialog over this popover; keep the popover open while it is.
  const toggle = (next: boolean) => {
    onBusy?.(next);
    setOpen(next);
  };
  return (
    <>
      <button type="button" className={pillClass} onClick={() => toggle(true)}>
        <Link2 className="size-3.5 text-muted-foreground" aria-hidden="true" />
        <span className={cn("truncate", !value && "text-muted-foreground")}>
          {value?.identifier ?? t(($) => $.wakeups.create.choose_issue)}
        </span>
      </button>
      <IssuePickerModal
        open={open}
        onOpenChange={toggle}
        title={t(($) => $.wakeups.create.pick_issue_title)}
        description={t(($) => $.wakeups.create.pick_issue_description)}
        excludeIds={issueId ? [issueId] : []}
        onSelect={(issue) => {
          onChange({ id: issue.id, identifier: issue.identifier });
          toggle(false);
        }}
      />
    </>
  );
}
