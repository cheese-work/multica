"use client";

import { useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api, clientErrorMessage } from "@multica/core/api";
import { ApiError } from "@multica/core/api/client";
import {
  BackgroundAgentsSchema,
  type BackgroundAgentChain,
  type BackgroundAgents,
  type BackgroundAgentsRead,
} from "@multica/core/api/schemas";
import { useWorkspaceId } from "@multica/core/hooks";
import { useCurrentMember } from "@multica/core/permissions";
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
import {
  SettingsCard,
  SettingsReadOnlyNotice,
  SettingsRow,
  SettingsSection,
  SettingsTab,
} from "./settings-layout";
import { settingsHref } from "./settings-navigation";

// Select values cannot be "" (the empty selection); agent ids are UUIDs.
const NONE = "none";

// Compact joins this list when its personal override ships (CHE-4121).
const JOBS = ["dm_title", "quick_actions", "routing"] as const;
type Job = (typeof JOBS)[number];
type Slot = "agent_id" | "fallback_agent_id";

/**
 * Workspace picks for the background jobs: one row per job, an agent and a
 * fallback that takes over on a provider error. The server stores one object
 * and a save replaces it, so every save starts from the latest stored value
 * and keeps the jobs and routing extras (statuses, overrides) this form does
 * not edit. A save presents the ETag it was built from; when another owner
 * saved first the server answers 409 and nothing is stored, and the owner
 * compares both versions before keeping theirs or applying their own again.
 * Owners and admins edit; members read.
 */
export function BackgroundAgentsTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const { role } = useCurrentMember(wsId);
  const canEdit = role === "owner" || role === "admin";
  const navigation = useNavigation();
  const qc = useQueryClient();
  const queryKey = ["background-agents", wsId];
  const { data, isFetching, isError, refetch } = useQuery({
    queryKey,
    queryFn: () => api.getBackgroundAgents(wsId),
    staleTime: 0,
    refetchOnMount: "always",
  });
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const queue = useRef<Promise<unknown>>(Promise.resolve());
  const [conflict, setConflict] = useState<{ job: Job; change: Partial<Record<Slot, string>>; current: BackgroundAgents } | null>(null);
  // A malformed response resolves to null: unreadable, never "nothing set".
  const unreadable = isError || (data === null && !isFetching);
  const value = unreadable ? undefined : data?.value;
  // A save built from a value that failed to load could erase the real one.
  const writable = canEdit && !!value && !isFetching && !conflict;

  const nameOf = (id: string) =>
    id === "" ? t(($) => $.background_agents.none)
      : (agents.find((a) => a.id === id)?.name ?? t(($) => $.background_agents.unknown_agent));

  function save(job: Job, change: Partial<Record<Slot, string>>) {
    const run = async () => {
      const latest = qc.getQueryData<BackgroundAgentsRead | null>(queryKey);
      if (!latest) return;
      const next = { ...latest.value, [job]: { ...latest.value[job], ...change } };
      // An unset slot goes out as null; the server rejects an empty string.
      const wire = Object.fromEntries(
        Object.entries(next).map(([key, chain]) => [
          key,
          {
            ...chain,
            agent_id: chain.agent_id || null,
            fallback_agent_id: chain.fallback_agent_id || null,
          },
        ]),
      );
      try {
        const saved = await api.updateBackgroundAgents(wsId, wire, latest.etag);
        if (saved) qc.setQueryData(queryKey, saved);
        else await qc.invalidateQueries({ queryKey });
      } catch (error) {
        const theirs = error instanceof ApiError && error.status === 409 ? conflictOf(error.body) : null;
        if (theirs) {
          // Show their value now; the owner's change waits in the compare panel.
          qc.setQueryData<BackgroundAgentsRead>(queryKey, theirs);
          setConflict({ job, change, current: theirs.value });
          return;
        }
        toast.error(clientErrorMessage(error) ?? t(($) => $.background_agents.toast_save_failed));
        await qc.invalidateQueries({ queryKey });
      }
    };
    queue.current = queue.current.then(run);
  }

  function picker(job: Job, chain: BackgroundAgentChain | undefined, slot: Slot, label: string, hint?: string) {
    const current = chain?.[slot] ?? "";
    const jobLabel = t(($) => $.background_agents.jobs[job].label);
    const name = `${jobLabel}: ${label}`;
    const pickAgentFirst = slot === "fallback_agent_id" && !chain?.agent_id;
    const items = [
      { value: NONE, label: t(($) => $.background_agents.none) },
      ...agents.map((a) => ({ value: a.id, label: a.name })),
    ];
    return (
      <SettingsRow
        anchor={`background-agents-${job}-${slot === "agent_id" ? "agent" : "fallback"}`}
        label={label}
        description={pickAgentFirst ? t(($) => $.background_agents.pick_agent_first) : hint}
        size={canEdit ? "select-wide" : undefined}
      >
        {canEdit ? (
          <Select
            items={items}
            value={current === "" ? NONE : current}
            disabled={!writable || pickAgentFirst}
            onValueChange={(next) => {
              const id = next === NONE ? "" : (next ?? "");
              if (id === current) return;
              // The fallback only runs when the agent fails, so it goes with it.
              save(job, slot === "agent_id" && id === "" ? { agent_id: "", fallback_agent_id: "" } : { [slot]: id });
            }}
          >
            <SelectTrigger size="sm" className="w-full" aria-label={name}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent align="end">
              <SelectItem value={NONE}>{t(($) => $.background_agents.none)}</SelectItem>
              {agents
                .filter((a) => !a.archived_at)
                .map((a) => {
                  // A workspace job needs an agent the whole workspace can run.
                  const narrowed = a.visibility !== "workspace";
                  return (
                    <SelectItem key={a.id} value={a.id} disabled={narrowed}>
                      {a.name}
                      {narrowed ? (
                        <span className="ml-2 text-caption text-muted-foreground">
                          {t(($) => $.background_agents.access_narrowed)}
                        </span>
                      ) : null}
                    </SelectItem>
                  );
                })}
            </SelectContent>
          </Select>
        ) : (
          <p className="text-body text-foreground">{nameOf(current)}</p>
        )}
      </SettingsRow>
    );
  }

  const slotsOf = (chain: BackgroundAgentChain | undefined) => (
    <dl className="grid grid-cols-[auto_1fr] gap-x-3 text-caption">
      <dt className="text-muted-foreground">{t(($) => $.background_agents.agent_label)}</dt>
      <dd>{nameOf(chain?.agent_id ?? "")}</dd>
      <dt className="text-muted-foreground">{t(($) => $.background_agents.fallback_label)}</dt>
      <dd>{nameOf(chain?.fallback_agent_id ?? "")}</dd>
    </dl>
  );

  const personalHref = settingsHref(navigation.pathname, navigation.searchParams, "personal-background-agents");

  return (
    <SettingsTab
      title={t(($) => $.background_agents.title)}
      description={t(($) => $.background_agents.description)}
      scope="workspace"
    >
      {!canEdit ? <SettingsReadOnlyNotice wsId={wsId} /> : null}
      {unreadable ? (
        <div role="alert" className="flex items-center justify-between gap-3 text-caption text-destructive">
          <span>{t(($) => $.background_agents.load_error)}</span>
          <Button type="button" size="sm" variant="outline" onClick={() => void refetch()}>
            {t(($) => $.background_agents.retry)}
          </Button>
        </div>
      ) : null}
      {conflict ? (
        <div role="alert" className="space-y-3 rounded-lg border border-destructive/40 p-3 text-caption">
          <p>
            <span className="font-medium">{t(($) => $.background_agents.conflict_title)}</span>{" "}
            {t(($) => $.background_agents.conflict_body)}
          </p>
          <p className="font-medium">{t(($) => $.background_agents.jobs[conflict.job].label)}</p>
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1">
              <p className="text-muted-foreground">{t(($) => $.background_agents.conflict_mine)}</p>
              {slotsOf({ ...conflict.current[conflict.job], ...conflict.change } as BackgroundAgentChain)}
            </div>
            <div className="space-y-1">
              <p className="text-muted-foreground">{t(($) => $.background_agents.conflict_theirs)}</p>
              {slotsOf(conflict.current[conflict.job])}
            </div>
          </div>
          <div className="flex flex-wrap gap-2">
            <Button type="button" size="sm" variant="outline" onClick={() => setConflict(null)}>
              {t(($) => $.background_agents.conflict_keep)}
            </Button>
            <Button
              type="button"
              size="sm"
              onClick={() => {
                const { job, change } = conflict;
                setConflict(null);
                save(job, change);
              }}
            >
              {t(($) => $.background_agents.conflict_apply)}
            </Button>
          </div>
        </div>
      ) : null}
      {!unreadable && !value ? (
        <p role="status" className="text-caption text-muted-foreground">
          {t(($) => $.background_agents.loading)}
        </p>
      ) : null}
      <p className="text-caption leading-5 text-muted-foreground">
        {t(($) => $.background_agents.personal_note_before)}{" "}
        <AppLink href={personalHref} className="underline underline-offset-2">
          {t(($) => $.background_agents.personal_link)}
        </AppLink>
        {t(($) => $.background_agents.personal_note_after)}
      </p>
      {(value ? JOBS : []).map((job) => (
        <SettingsSection
          key={job}
          anchor={`background-agents-${job}`}
          title={t(($) => $.background_agents.jobs[job].label)}
          description={t(($) => $.background_agents.jobs[job].hint)}
          scope={job === "routing" ? "workspace-only" : undefined}
        >
          <p className="px-0.5 text-caption leading-5 text-muted-foreground">
            {t(($) => $.background_agents.jobs[job].no_agent)}
          </p>
          <SettingsCard>
            {picker(job, value?.[job], "agent_id", t(($) => $.background_agents.agent_label))}
            {picker(
              job,
              value?.[job],
              "fallback_agent_id",
              t(($) => $.background_agents.fallback_label),
              t(($) => $.background_agents.fallback_hint),
            )}
          </SettingsCard>
        </SettingsSection>
      ))}
    </SettingsTab>
  );
}

// The 409 body carries the value that won and its ETag.
function conflictOf(body: unknown): BackgroundAgentsRead | null {
  const { current, etag } = (body ?? {}) as { current?: unknown; etag?: unknown };
  const parsed = BackgroundAgentsSchema.safeParse(current);
  return parsed.success ? { value: parsed.data, etag: typeof etag === "string" ? etag : null } : null;
}
