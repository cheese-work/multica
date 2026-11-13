"use client";

import { useRef } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import type { RoutingAgentSetting } from "@multica/core/api/schemas";
import { agentListOptions } from "@multica/core/workspace/queries";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { useT } from "../../i18n";
import { SettingsRow } from "./settings-layout";

// Select values cannot be "" (the empty selection); agent ids are UUIDs.
const NONE = "none";

/**
 * CHE-3627: the agent that handles routing-only workflow steps, and the one
 * that takes over when it hits a provider error. The server stores one
 * object and a save replaces it, so each save starts from the latest stored
 * value and keeps the statuses and overrides this form does not edit.
 * Overrides per workflow step are set through the CLI.
 */
export function RoutingAgentRows({ wsId, canEdit }: { wsId: string; canEdit: boolean }) {
  const { t } = useT("settings");
  const qc = useQueryClient();
  const queryKey = ["routing-agent", wsId];
  const { data: setting, isFetching, isError } = useQuery({
    queryKey,
    queryFn: () => api.getRoutingAgent(wsId),
    staleTime: 0,
    refetchOnMount: "always",
  });
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const queue = useRef<Promise<unknown>>(Promise.resolve());
  // A save built from a value that failed to load could erase the real one.
  const writable = canEdit && !!setting && !isFetching && !isError;

  const nameOf = (id: string) =>
    id === "" ? t(($) => $.workspace.routing_agent_none)
      : (agents.find((a) => a.id === id)?.name ?? t(($) => $.workspace.routing_agent_unknown));
  const items = [
    { value: NONE, label: t(($) => $.workspace.routing_agent_none) },
    ...agents.map((a) => ({ value: a.id, label: a.name })),
  ];

  function save(change: Partial<Pick<RoutingAgentSetting, "agent_id" | "fallback_agent_id">>) {
    const run = async () => {
      const latest = qc.getQueryData<RoutingAgentSetting | null>(queryKey);
      if (!latest) return;
      try {
        qc.setQueryData(queryKey, await api.updateRoutingAgent(wsId, { ...latest, ...change }));
      } catch (error) {
        toast.error(error instanceof Error ? error.message : t(($) => $.workspace.toast_save_failed));
      }
    };
    queue.current = queue.current.then(run);
  }

  function row(
    anchor: string,
    label: string,
    hint: string,
    field: "agent_id" | "fallback_agent_id",
  ) {
    const current = setting?.[field] ?? "";
    return (
      <SettingsRow anchor={anchor} label={label} description={hint} size={canEdit ? "select" : undefined}>
        {canEdit ? (
          <Select
            items={items}
            value={current === "" ? NONE : current}
            disabled={!writable}
            onValueChange={(next) => {
              const id = next === NONE ? "" : (next ?? "");
              if (id !== current) save({ [field]: id });
            }}
          >
            <SelectTrigger size="sm" className="w-full" aria-label={label}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent align="end">
              <SelectItem value={NONE}>{t(($) => $.workspace.routing_agent_none)}</SelectItem>
              {agents
                .filter((a) => !a.archived_at)
                .map((a) => (
                  <SelectItem key={a.id} value={a.id}>{a.name}</SelectItem>
                ))}
            </SelectContent>
          </Select>
        ) : (
          <p className="text-body text-foreground">{nameOf(current)}</p>
        )}
      </SettingsRow>
    );
  }

  return (
    <>
      {row(
        "routing-agent",
        t(($) => $.workspace.routing_agent_label),
        t(($) => $.workspace.routing_agent_hint),
        "agent_id",
      )}
      {row(
        "routing-fallback-agent",
        t(($) => $.workspace.routing_fallback_label),
        t(($) => $.workspace.routing_fallback_hint),
        "fallback_agent_id",
      )}
    </>
  );
}
