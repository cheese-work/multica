"use client";

import { useMemo, useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { Bot, ChevronDown } from "@multica/ui/icons";
import { isAgentRuntimeBound } from "@multica/core/agents";
import { agentListOptions, memberListOptions, squadListOptions } from "@multica/core/workspace/queries";
import { cn } from "@multica/ui/lib/utils";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@multica/ui/components/ui/dropdown-menu";
import { ActorAvatar } from "../../common/actor-avatar";
import { useT } from "../../i18n";
import { matchesPinyin } from "../../editor/extensions/pinyin-match";
import { PickerEmpty, PickerItem, PickerSection, PropertyPicker } from "./pickers/property-picker";
import type { WakeupDraft } from "./wakeup-draft";

export const pillClass =
  "inline-flex h-7 max-w-full min-w-0 items-center gap-1.5 rounded-md border border-surface-border bg-surface px-2 text-label outline-none hover:bg-accent focus-visible:ring-3 focus-visible:ring-ring/50 data-popup-open:bg-accent [&_svg]:shrink-0";

export function PillMenu({
  label,
  value,
  options,
  onChange,
  placeholder,
}: {
  label: string;
  value: string;
  options: { value: string; label: string }[];
  onChange: (value: string) => void;
  /** Shown until a value is chosen. */
  placeholder?: string;
}) {
  const current = options.find((option) => option.value === value);
  return (
    <DropdownMenu>
      <DropdownMenuTrigger render={<button type="button" className={pillClass} aria-label={`${label}: ${current?.label ?? ""}`} />}>
        <span className={cn("truncate", !current && "text-muted-foreground")}>{current?.label ?? placeholder}</span>
        <ChevronDown className="size-3 text-muted-foreground" aria-hidden="true" />
      </DropdownMenuTrigger>
      <DropdownMenuContent className="max-h-80">
        <DropdownMenuRadioGroup value={value} onValueChange={(next) => onChange(String(next))}>
          {options.map((option) => (
            <DropdownMenuRadioItem key={option.value} value={option.value}>
              {option.label}
            </DropdownMenuRadioItem>
          ))}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

export function Segmented<T extends string>({
  label,
  value,
  options,
  onChange,
}: {
  label: string;
  value: T;
  options: { value: T; label: string }[];
  onChange: (value: T) => void;
}) {
  return (
    <div role="radiogroup" aria-label={label} className="inline-flex w-fit gap-0.5 rounded-lg bg-muted p-0.5">
      {options.map((option) => (
        <button
          key={option.value}
          type="button"
          role="radio"
          aria-checked={value === option.value}
          onClick={() => onChange(option.value)}
          className={cn(
            "h-6 rounded-md px-2.5 text-caption text-muted-foreground outline-none focus-visible:ring-3 focus-visible:ring-ring/50",
            value === option.value && "bg-surface font-medium text-foreground shadow-[var(--surface-shadow)]",
          )}
        >
          {option.label}
        </button>
      ))}
    </div>
  );
}

function useMatches() {
  const [filter, setFilter] = useState("");
  const query = filter.trim().toLowerCase();
  const matches = (name: string) => !query || name.toLowerCase().includes(query) || matchesPinyin(name, query);
  return { setFilter, matches };
}

export function AgentChoice({
  workspaceId,
  value,
  onChange,
  placeholder,
  allowAny = false,
}: {
  workspaceId: string;
  value: string;
  onChange: (id: string) => void;
  placeholder: string;
  allowAny?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const { setFilter, matches } = useMatches();
  const { data = [] } = useQuery(agentListOptions(workspaceId));
  const agents = useMemo(() => data.filter((a) => !a.archived_at), [data]);
  const selected = agents.find((a) => a.id === value);
  const pick = (id: string) => {
    onChange(id);
    setOpen(false);
  };
  return (
    <PropertyPicker
      open={open}
      onOpenChange={setOpen}
      width="w-56"
      align="start"
      searchable
      onSearchChange={setFilter}
      triggerRender={<button type="button" className={pillClass} />}
      trigger={
        <>
          {selected ? (
            <ActorAvatar actorType="agent" actorId={selected.id} size="sm" />
          ) : (
            <Bot className="size-3.5 text-muted-foreground" aria-hidden="true" />
          )}
          <span className={cn("truncate", !selected && "text-muted-foreground")}>{selected?.name ?? placeholder}</span>
          <ChevronDown className="size-3 text-muted-foreground" aria-hidden="true" />
        </>
      }
    >
      {allowAny && (
        <PickerItem selected={!value} onClick={() => pick("")}>
          <Bot className="size-3.5 text-muted-foreground" aria-hidden="true" />
          <span className="truncate">{placeholder}</span>
        </PickerItem>
      )}
      {agents.filter((a) => matches(a.name)).length === 0 && !allowAny ? (
        <PickerEmpty />
      ) : (
        agents
          .filter((a) => matches(a.name))
          .map((a) => (
            <PickerItem
              key={a.id}
              selected={a.id === value}
              disabled={!allowAny && !isAgentRuntimeBound(a)}
              onClick={() => pick(a.id)}
            >
              <ActorAvatar actorType="agent" actorId={a.id} size="sm" showStatusDot />
              <span className="truncate">{a.name}</span>
            </PickerItem>
          ))
      )}
    </PropertyPicker>
  );
}

export function ActorChoice({
  workspaceId,
  value,
  onChange,
}: {
  workspaceId: string;
  value: WakeupDraft["replyActor"];
  onChange: (value: WakeupDraft["replyActor"]) => void;
}) {
  const { t } = useT("issues");
  const [open, setOpen] = useState(false);
  const { setFilter, matches } = useMatches();
  const { data: members = [] } = useQuery(memberListOptions(workspaceId));
  const { data: agentRows = [] } = useQuery(agentListOptions(workspaceId));
  const agents = agentRows.filter((a) => !a.archived_at);
  const name =
    value?.type === "member"
      ? members.find((m) => m.user_id === value.id)?.name
      : value?.type === "agent"
        ? agents.find((a) => a.id === value.id)?.name
        : undefined;
  const pick = (next: WakeupDraft["replyActor"]) => {
    onChange(next);
    setOpen(false);
  };
  const section = (label: string, rows: ReactNode[]) =>
    rows.length > 0 && <PickerSection label={label}>{rows}</PickerSection>;
  return (
    <PropertyPicker
      open={open}
      onOpenChange={setOpen}
      width="w-56"
      align="start"
      searchable
      onSearchChange={setFilter}
      triggerRender={<button type="button" className={pillClass} />}
      trigger={
        <>
          {value ? (
            <ActorAvatar actorType={value.type} actorId={value.id} size="sm" />
          ) : null}
          <span className="truncate">{name ?? t(($) => $.wakeups.create.anyone)}</span>
          <ChevronDown className="size-3 text-muted-foreground" aria-hidden="true" />
        </>
      }
    >
      <PickerItem selected={!value} onClick={() => pick(null)}>
        <span className="truncate">{t(($) => $.wakeups.create.anyone)}</span>
      </PickerItem>
      {section(
        t(($) => $.wakeups.create.members),
        members
          .filter((m) => matches(m.name))
          .map((m) => (
            <PickerItem
              key={m.user_id}
              selected={value?.type === "member" && value.id === m.user_id}
              onClick={() => pick({ type: "member", id: m.user_id })}
            >
              <ActorAvatar actorType="member" actorId={m.user_id} size="sm" />
              <span className="truncate">{m.name}</span>
            </PickerItem>
          )),
      )}
      {section(
        t(($) => $.wakeups.create.agents),
        agents
          .filter((a) => matches(a.name))
          .map((a) => (
            <PickerItem
              key={a.id}
              selected={value?.type === "agent" && value.id === a.id}
              onClick={() => pick({ type: "agent", id: a.id })}
            >
              <ActorAvatar actorType="agent" actorId={a.id} size="sm" />
              <span className="truncate">{a.name}</span>
            </PickerItem>
          )),
      )}
    </PropertyPicker>
  );
}

export function AssigneeChoice({
  workspaceId,
  value,
  onChange,
}: {
  workspaceId: string;
  value: WakeupDraft["assignee"];
  onChange: (value: WakeupDraft["assignee"]) => void;
}) {
  const { t } = useT("issues");
  const [open, setOpen] = useState(false);
  const { setFilter, matches } = useMatches();
  const { data: members = [] } = useQuery(memberListOptions(workspaceId));
  const { data: agentRows = [] } = useQuery(agentListOptions(workspaceId));
  const { data: squads = [] } = useQuery(squadListOptions(workspaceId));
  const agents = agentRows.filter((a) => !a.archived_at);
  const name =
    value?.type === "member"
      ? members.find((m) => m.user_id === value.id)?.name
      : value?.type === "agent"
        ? agents.find((a) => a.id === value.id)?.name
        : value?.type === "squad"
          ? squads.find((s) => s.id === value.id)?.name
          : undefined;
  const pick = (next: WakeupDraft["assignee"]) => {
    onChange(next);
    setOpen(false);
  };
  const section = (label: string, rows: ReactNode[]) =>
    rows.length > 0 && <PickerSection label={label}>{rows}</PickerSection>;
  return (
    <PropertyPicker
      open={open}
      onOpenChange={setOpen}
      width="w-56"
      align="start"
      searchable
      onSearchChange={setFilter}
      triggerRender={<button type="button" className={pillClass} />}
      trigger={
        <>
          {value && value.type !== "squad" ? <ActorAvatar actorType={value.type} actorId={value.id} size="sm" /> : null}
          <span className={cn("truncate", !name && "text-muted-foreground")}>{name ?? t(($) => $.wakeups.create.choose_assignee)}</span>
          <ChevronDown className="size-3 text-muted-foreground" aria-hidden="true" />
        </>
      }
    >
      {section(
        t(($) => $.wakeups.create.members),
        members
          .filter((m) => matches(m.name))
          .map((m) => (
            <PickerItem
              key={m.user_id}
              selected={value?.type === "member" && value.id === m.user_id}
              onClick={() => pick({ type: "member", id: m.user_id })}
            >
              <ActorAvatar actorType="member" actorId={m.user_id} size="sm" />
              <span className="truncate">{m.name}</span>
            </PickerItem>
          )),
      )}
      {section(
        t(($) => $.wakeups.create.agents),
        agents
          .filter((a) => matches(a.name))
          .map((a) => (
            <PickerItem key={a.id} selected={value?.type === "agent" && value.id === a.id} onClick={() => pick({ type: "agent", id: a.id })}>
              <ActorAvatar actorType="agent" actorId={a.id} size="sm" />
              <span className="truncate">{a.name}</span>
            </PickerItem>
          )),
      )}
      {section(
        t(($) => $.wakeups.create.squads),
        squads
          .filter((s) => matches(s.name))
          .map((s) => (
            <PickerItem key={s.id} selected={value?.type === "squad" && value.id === s.id} onClick={() => pick({ type: "squad", id: s.id })}>
              <span className="truncate">{s.name}</span>
            </PickerItem>
          )),
      )}
    </PropertyPicker>
  );
}
