"use client";

import React, { useMemo, useState } from "react";
import { Plus } from "@multica/ui/icons";
import { Button } from "@multica/ui/components/ui/button";
import { IconButton } from "@multica/ui/components/common/icon-button";
import { Tooltip, TooltipTrigger, TooltipContent } from "@multica/ui/components/ui/tooltip";
import { ActorAvatar } from "../../common/actor-avatar";
import {
  PickerEmpty,
  PickerItem,
  PickerSection,
  PropertyPicker,
} from "../../issues/components/pickers/property-picker";
import { matchesPinyin } from "../../editor/extensions/pinyin-match";
import type { Agent, ChatSession } from "@multica/core/types";
import { pickRecentAgentIds } from "@multica/core/chat/recent-agents";
import { useRecentAgentsSettingsStore } from "@multica/core/chat/recent-agents-settings-store";
import { isAgentRuntimeBound } from "@multica/core/agents";
import { toast } from "sonner";
import { useT } from "../../i18n";

/**
 * Agent picker: a searchable, grouped (Recent / My agents / Others) list of agents in a
 * PropertyPicker. The caller supplies the trigger. `recentAgentIds` (opt-in)
 * adds a "Recent" section above them; those agents leave the other two. `currentAgentId` marks one
 * agent with a check — omit it (as "new chat" does) when there is no current
 * selection to highlight.
 */
export function AgentPicker({
  agents,
  userId,
  currentAgentId,
  recentAgentIds,
  onSelect,
  trigger,
  triggerRender,
  tooltip,
  side = "bottom",
  align = "start",
}: {
  agents: Agent[];
  userId: string | undefined;
  currentAgentId?: string;
  recentAgentIds?: readonly string[];
  onSelect: (agent: Agent) => void;
  trigger: React.ReactNode;
  triggerRender: React.ReactElement;
  tooltip?: React.ReactNode;
  side?: "top" | "bottom";
  align?: "start" | "center" | "end";
}) {
  const { t } = useT("chat");
  const [open, setOpen] = useState(false);
  const [filter, setFilter] = useState("");
  const { recent, mine, others } = useMemo(() => {
    const recentIds = new Set(recentAgentIds);
    const byId = new Map(agents.map((a) => [a.id, a]));
    const recent = (recentAgentIds ?? []).flatMap((id) => byId.get(id) ?? []);
    const mine: Agent[] = [];
    const others: Agent[] = [];
    for (const a of agents) {
      if (recentIds.has(a.id)) continue;
      if (a.owner_id === userId) mine.push(a);
      else others.push(a);
    }
    return { recent, mine, others };
  }, [agents, userId, recentAgentIds]);

  const query = filter.trim().toLowerCase();
  const matches = (name: string) =>
    !query || name.toLowerCase().includes(query) || matchesPinyin(name, query);
  const sections = [
    { key: "recent", label: t(($) => $.window.recent), items: recent.filter((a) => matches(a.name)) },
    { key: "mine", label: t(($) => $.window.my_agents), items: mine.filter((a) => matches(a.name)) },
    { key: "others", label: t(($) => $.window.others), items: others.filter((a) => matches(a.name)) },
  ].filter((section) => section.items.length > 0);

  const handlePick = (agent: Agent) => {
    onSelect(agent);
    setOpen(false);
  };

  return (
    <PropertyPicker
      open={open}
      onOpenChange={setOpen}
      width="w-64"
      align={align}
      side={side}
      searchable
      searchPlaceholder={t(($) => $.window.agent_filter_placeholder)}
      onSearchChange={setFilter}
      triggerRender={triggerRender}
      trigger={trigger}
      tooltip={tooltip}
    >
      {sections.length === 0 ? (
        <PickerEmpty />
      ) : (
        sections.map((section) => (
          <PickerSection key={section.key} label={section.label}>
            {section.items.map((agent) => (
              <AgentPickerItem
                key={agent.id}
                agent={agent}
                isCurrent={agent.id === currentAgentId}
                onSelect={handlePick}
              />
            ))}
          </PickerSection>
        ))
      )}
    </PropertyPicker>
  );
}

function AgentPickerItem({
  agent,
  isCurrent,
  onSelect,
}: {
  agent: Agent;
  isCurrent: boolean;
  onSelect: (agent: Agent) => void;
}) {
  const { t } = useT("chat");
  const runtimeBound = isAgentRuntimeBound(agent);
  return (
    <PickerItem
      selected={isCurrent}
      disabled={!runtimeBound}
      tooltip={
        runtimeBound ? undefined : t(($) => $.window.agent_needs_runtime_hint)
      }
      onClick={() => onSelect(agent)}
    >
      <ActorAvatar
        actorType="agent"
        actorId={agent.id}
        size="md"
        enableHoverCard
        showStatusDot
      />
      <span className="truncate flex-1">{agent.name}</span>
      {!runtimeBound && (
        <span className="shrink-0 text-micro text-amber-600 dark:text-amber-400">
          {t(($) => $.window.agent_needs_runtime)}
        </span>
      )}
    </PickerItem>
  );
}

/**
 * "New chat" ⊕ button. Per the Chat V2 design, starting a new chat is where the
 * agent is chosen — so this opens an AgentPicker and reports the pick via
 * `onStart`. No agent is pre-checked: a new chat has no "current" agent yet.
 * Shortcuts: with a single available agent it starts immediately (no point
 * showing a one-item menu); with none it still fires `onStart(null)` so the
 * surface shows its no-agent empty state.
 */
export function NewChatButton({
  agents,
  userId,
  sessions,
  onStart,
  side = "bottom",
}: {
  agents: Agent[];
  userId: string | undefined;
  /** All the user's chat sessions (archived included); drives "Recent". */
  sessions?: readonly ChatSession[];
  onStart: (agent: Agent | null) => void;
  side?: "top" | "bottom";
}) {
  const { t } = useT("chat");
  const label = t(($) => $.window.new_chat_tooltip);
  const showRecent = useRecentAgentsSettingsStore((s) => s.showRecentAgents);
  const recentAgentIds = useMemo(
    () => (showRecent && userId && sessions ? pickRecentAgentIds(sessions, agents, userId, Date.now()) : []),
    [showRecent, userId, sessions, agents],
  );

  if (agents.length <= 1) {
    const only = agents[0] ?? null;
    return (
      <Tooltip>
        <TooltipTrigger
          render={
            <Button
              variant="ghost"
              size="icon-sm"
              className="rounded-full text-muted-foreground"
              aria-label={label}
              onClick={() => {
                if (only && !isAgentRuntimeBound(only)) {
                  toast.error(t(($) => $.input.runtime_required_toast));
                  return;
                }
                onStart(only);
              }}
            />
          }
        >
          <Plus />
        </TooltipTrigger>
        <TooltipContent side={side === "top" ? "top" : "bottom"}>{label}</TooltipContent>
      </Tooltip>
    );
  }

  return (
    <AgentPicker
      agents={agents}
      userId={userId}
      recentAgentIds={recentAgentIds}
      onSelect={(agent) => onStart(agent)}
      side={side}
      align="start"
      triggerRender={
        <IconButton
          variant="ghost"
          size="icon-sm"
          className="rounded-full text-muted-foreground"
          label={label}
          tooltipSide={side === "top" ? "top" : "bottom"}
        />
      }
      trigger={<Plus />}
    />
  );
}
