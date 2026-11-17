"use client";

import { useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api, clientErrorMessage } from "@multica/core/api";
import type { BackgroundAgents } from "@multica/core/api/schemas";
import { useWorkspaceId } from "@multica/core/hooks";
import { agentListOptions } from "@multica/core/workspace/queries";
import { Button } from "@multica/ui/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { AppLink, useNavigation } from "../../navigation";
import { useT } from "../../i18n";
import { SettingsCard, SettingsRow, SettingsSection, SettingsTab } from "./settings-layout";
import { settingsHref } from "./settings-navigation";
import { useOnline } from "./use-online";

// Select values cannot be "" (the empty selection); agent ids are UUIDs.
const NONE = "none";

// Compact joins this list when its personal override ships (CHE-4121).
// Routing is workspace-only and never appears here.
const JOBS = ["dm_title", "quick_actions"] as const;
type Job = (typeof JOBS)[number];
type Slot = "agent_id" | "fallback_agent_id";

/**
 * The member's own picks for the background jobs. A personal pick wins over
 * the workspace pick; an unset row shows the workspace agent it falls back to.
 * The server merges a save per job and field, so a save sends only what
 * changed and a null clears the pick. Every member edits their own picks.
 */
export function PersonalBackgroundAgentsTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const navigation = useNavigation();
  const qc = useQueryClient();
  const queryKey = ["background-agents", wsId, "me"];
  const online = useOnline();
  const { data, isFetching, isError, refetch } = useQuery({
    queryKey,
    queryFn: () => api.getMyBackgroundAgents(wsId),
    staleTime: 0,
    refetchOnMount: "always",
    // Focus is repo-wide off. Here a refocus is how a save made in another tab shows up.
    refetchOnWindowFocus: "always",
  });
  // Only you write your picks, so a read that differs from what this tab last knew
  // came from another tab of yours. A null seed means "the next read is not news".
  const known = useRef<string | null>(null);
  const [anotherTab, setAnotherTab] = useState(false);
  useEffect(() => {
    if (!data) return;
    const next = JSON.stringify(data);
    if (known.current !== null && known.current !== next) setAnotherTab(true);
    known.current = next;
  }, [data]);
  // Members can read the workspace picks; they only show what an unset row uses.
  const { data: workspace } = useQuery({
    queryKey: ["background-agents", wsId],
    queryFn: () => api.getBackgroundAgents(wsId),
  });
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  // A malformed response resolves to null and a rejected read (503, offline) is an error:
  // both are unreadable, never "nothing set" and never an endless loading line.
  const unreadable = (isError && !data) || (data === null && !isFetching);
  const mine: BackgroundAgents | undefined = data ?? undefined;

  const agentName = (id: string) => agents.find((a) => a.id === id)?.name ?? t(($) => $.background_agents.unknown_agent);

  async function save(patch: Record<string, Record<string, string | null>>) {
    try {
      const saved = await api.updateMyBackgroundAgents(wsId, patch);
      setAnotherTab(false);
      if (saved) {
        known.current = JSON.stringify(saved);
        qc.setQueryData(queryKey, saved);
      } else {
        known.current = null;
        await qc.invalidateQueries({ queryKey });
      }
    } catch (error) {
      toast.error(clientErrorMessage(error) ?? t(($) => $.personal_background_agents.toast_save_failed));
      await qc.invalidateQueries({ queryKey });
    }
  }

  function picker(job: Job, slot: Slot, label: string, workspaceName: string) {
    const current = mine?.[job]?.[slot] ?? "";
    const pickAgentFirst = slot === "fallback_agent_id" && !mine?.[job]?.agent_id;
    const none = slot === "agent_id" ? workspaceName : t(($) => $.background_agents.none);
    const items = [
      { value: NONE, label: none },
      ...agents.map((a) => ({
        value: a.id,
        label: a.archived_at ? `${a.name} · ${t(($) => $.personal_background_agents.archived)}` : a.name,
      })),
    ];
    return (
      <SettingsRow
        anchor={`personal-background-agents-${job}-${slot === "agent_id" ? "agent" : "fallback"}`}
        label={label}
        description={pickAgentFirst ? t(($) => $.background_agents.pick_agent_first) : undefined}
        size="select-wide"
      >
        <Select
          items={items}
          value={current === "" ? NONE : current}
          disabled={isFetching || !online || !mine || pickAgentFirst}
          onValueChange={(next) => {
            const id = next === NONE ? "" : (next ?? "");
            if (id === current) return;
            // The fallback only runs when the agent fails, so it goes with it.
            void save({
              [job]: slot === "agent_id" && id === "" ? { agent_id: null, fallback_agent_id: null } : { [slot]: id || null },
            });
          }}
        >
          <SelectTrigger
            size="sm"
            className="w-full"
            aria-label={`${t(($) => $.background_agents.jobs[job].label)}: ${label}`}
          >
            <SelectValue />
          </SelectTrigger>
          <SelectContent align="end">
            <SelectItem value={NONE}>
              {slot === "agent_id"
                ? t(($) => $.personal_background_agents.use_workspace_pick)
                : t(($) => $.background_agents.none)}
            </SelectItem>
            {agents
              .filter((a) => !a.archived_at)
              .map((a) => (
                <SelectItem key={a.id} value={a.id}>
                  {a.name}
                </SelectItem>
              ))}
          </SelectContent>
        </Select>
      </SettingsRow>
    );
  }

  const workspaceHref = settingsHref(navigation.pathname, navigation.searchParams, "background-agents");

  return (
    <SettingsTab
      title={t(($) => $.personal_background_agents.title)}
      description={t(($) => $.personal_background_agents.description)}
      scope="personal"
    >
      {unreadable ? (
        <div role="alert" className="flex items-center justify-between gap-3 text-caption text-destructive">
          <span>
            {t(($) => $.personal_background_agents.load_error)} {t(($) => $.personal_background_agents.load_error_detail)}
          </span>
          <Button type="button" size="sm" variant="outline" onClick={() => void refetch()}>
            {t(($) => $.background_agents.retry)}
          </Button>
        </div>
      ) : null}
      {!online ? (
        <div role="status" className="space-y-0.5 rounded-lg border p-3 text-caption">
          <p className="font-medium">{t(($) => $.personal_background_agents.offline_title)}</p>
          <p className="text-muted-foreground">{t(($) => $.personal_background_agents.offline_body)}</p>
        </div>
      ) : null}
      {anotherTab ? (
        <p role="status" className="rounded-lg border p-3 text-caption">
          {t(($) => $.personal_background_agents.another_tab)}
        </p>
      ) : null}
      {!unreadable && !mine ? (
        <p role="status" className="text-caption text-muted-foreground">
          {t(($) => $.background_agents.loading)}
        </p>
      ) : null}
      {(mine ? JOBS : []).map((job) => {
        const workspaceId = workspace?.value[job]?.agent_id ?? "";
        const workspaceAgent = workspace
          ? workspaceId === ""
            ? t(($) => $.background_agents.none)
            : agentName(workspaceId)
          : null;
        const personalId = mine?.[job]?.agent_id ?? "";
        const archived = personalId !== "" ? agents.find((a) => a.id === personalId && a.archived_at) : undefined;
        const jobLabel = t(($) => $.background_agents.jobs[job].label);
        return (
          <SettingsSection
            key={job}
            anchor={`personal-background-agents-${job}`}
            title={jobLabel}
            description={t(($) => $.personal_background_agents.jobs[job].hint)}
          >
            <p className="px-0.5 text-caption leading-5 text-muted-foreground">
              {t(($) => $.background_agents.jobs[job].no_agent)}
            </p>
            {archived ? (
              <div role="alert" className="space-y-2 rounded-lg border border-destructive/40 p-3 text-caption">
                <p>
                  {t(($) => $.personal_background_agents.archived_warning, { agent: archived.name, job: jobLabel })}
                </p>
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  disabled={!online}
                  onClick={() => void save({ [job]: { agent_id: null, fallback_agent_id: null } })}
                >
                  {t(($) => $.personal_background_agents.use_workspace_button, {
                    agent: workspaceAgent ?? t(($) => $.background_agents.none),
                  })}
                </Button>
              </div>
            ) : null}
            <SettingsCard>
              {picker(
                job,
                "agent_id",
                t(($) => $.background_agents.agent_label),
                workspaceAgent === null
                  ? t(($) => $.personal_background_agents.using_workspace_pick)
                  : t(($) => $.personal_background_agents.using_workspace, { agent: workspaceAgent }),
              )}
              {picker(job, "fallback_agent_id", t(($) => $.background_agents.fallback_label), "")}
            </SettingsCard>
          </SettingsSection>
        );
      })}
      <p className="text-caption leading-5 text-muted-foreground">
        {t(($) => $.personal_background_agents.routing_before)}{" "}
        <AppLink href={workspaceHref} className="underline underline-offset-2">
          {t(($) => $.personal_background_agents.routing_link)}
        </AppLink>
        {t(($) => $.personal_background_agents.routing_after)}
      </p>
    </SettingsTab>
  );
}
