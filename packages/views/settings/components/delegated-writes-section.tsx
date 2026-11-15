"use client";

import { useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { DelegatedWritesSchema, type DelegatedWrites } from "@multica/core/api/schemas";
import { useWorkspaceSettingsDraftStore } from "@multica/core/settings/draft-stores";
import { agentListOptions } from "@multica/core/workspace/queries";
import { AlertCircle, Check, Eye, Lock, X } from "@multica/ui/icons";
import { Button } from "@multica/ui/components/ui/button";
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import { Switch } from "@multica/ui/components/ui/switch";
import { useT } from "../../i18n";
import { SettingsCard, SettingsSection } from "./settings-layout";
import { useSettingsDraft } from "./use-settings-draft";

// The fields the server accepts (delegatedWriteFields in
// server/internal/handler/workspace_delegated_writes.go); it rejects any other.
const GOVERNED_FIELDS = [
  "workflow.steps", "workflow.instructions", "workspace.context",
  "squad.instructions", "agent.instructions", "project.description",
  "workflow.budget", "issue.workflow",
];

const toggle = (list: string[], item: string) =>
  list.includes(item) ? list.filter((x) => x !== item) : [...list, item];

/**
 * CHE-4617 (design: design/delegated-governed-writes, direction B): reads and
 * writes `delegated_governed_writes`. Owners and admins edit; members see the
 * same grid read-only. The edit is kept as a persisted draft (one JSON field)
 * until a save succeeds or Cancel discards it, so a failed save loses nothing.
 */
export function DelegatedWritesSection({ wsId, canEdit }: { wsId: string; canEdit: boolean }) {
  const { t } = useT("settings");
  const qc = useQueryClient();
  const queryKey = ["delegated-writes", wsId];
  const { data: saved, isFetching, isError } = useQuery({
    queryKey,
    queryFn: () => api.getDelegatedWrites(wsId),
    staleTime: 0,
    refetchOnMount: "always",
  });
  const { data: agents = [] } = useQuery(agentListOptions(wsId));

  const savedJson = useMemo(() => JSON.stringify(saved ?? null), [saved]);
  const [values, setDraft, confirmSent] = useSettingsDraft(
    useWorkspaceSettingsDraftStore,
    `${wsId}:delegated-writes`,
    useMemo(() => ({ rule: savedJson }), [savedJson]),
  );
  const dirty = values.rule !== savedJson;
  const [opened, setOpened] = useState(false);
  const [confirmed, setConfirmed] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const heading = t(($) => $.delegated_writes.title);
  const shell = (action: React.ReactNode, body: React.ReactNode) => (
    <SettingsSection title={heading} description={t(($) => $.delegated_writes.description)} action={action}>
      <SettingsCard>
        <div className="space-y-4 px-4 py-4">{body}</div>
      </SettingsCard>
    </SettingsSection>
  );

  // A rule that failed to load is never edited: a save would replace the real one.
  if (!saved) {
    return shell(null, (
      <p className="text-caption text-muted-foreground">
        {isFetching && !isError ? "" : t(($) => $.delegated_writes.load_failed)}
      </p>
    ));
  }

  const editing = canEdit && (opened || dirty);
  let rule: DelegatedWrites = saved;
  if (dirty) {
    try {
      const parsed = DelegatedWritesSchema.safeParse(JSON.parse(values.rule));
      if (parsed.success) rule = parsed.data;
    } catch {
      // an unreadable draft follows the saved rule
    }
  }
  const nameOf = (id: string) => agents.find((a) => a.id === id)?.name ?? t(($) => $.delegated_writes.unknown_agent);
  const empty = rule.allowed_agents.length === 0 && rule.allowed_fields.length === 0;
  const needsConfirm = !rule.requires_human_decision && saved.requires_human_decision;
  const edit = (next: Partial<DelegatedWrites>) => setDraft({ rule: JSON.stringify({ ...rule, ...next }) });

  const cancel = () => {
    setDraft({ rule: undefined });
    setOpened(false);
    setConfirmed(false);
    setError(null);
  };
  const save = async () => {
    const sent = JSON.stringify(rule);
    setSaving(true);
    setError(null);
    try {
      qc.setQueryData(queryKey, await api.updateDelegatedWrites(wsId, rule));
      confirmSent({ rule: sent });
      setOpened(false);
      setConfirmed(false);
    } catch (e) {
      // The draft stays; the user fixes the cause and saves again.
      setError(e instanceof Error ? e.message : t(($) => $.delegated_writes.save_failed));
    } finally {
      setSaving(false);
    }
  };

  const direct = !rule.requires_human_decision;
  const summary = t(
    ($) => (direct ? $.delegated_writes.summary_direct : $.delegated_writes.summary_decision),
    {
      agents: t(($) => $.delegated_writes.agents_count, { count: rule.allowed_agents.length }),
      fields: t(($) => $.delegated_writes.fields_count, { count: rule.allowed_fields.length }),
    },
  );
  const isNew = (agentId: string, field: string) =>
    editing && !(saved.allowed_agents.includes(agentId) && saved.allowed_fields.includes(field));
  const addable = agents.filter((a) => !a.archived_at && !rule.allowed_agents.includes(a.id));

  const grid = (
    <div className="overflow-x-auto">
      <table className="w-full border-collapse text-caption">
        <thead>
          <tr className="text-left text-muted-foreground">
            <th scope="col" className="py-1.5 pr-3 font-medium">{t(($) => $.delegated_writes.col_agent)}</th>
            {GOVERNED_FIELDS.map((f) => (
              <th key={f} scope="col" className="px-2 py-1.5 font-mono font-medium">{f}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rule.allowed_agents.map((id) => (
            <tr key={id} className="border-t border-surface-border">
              <th scope="row" className="py-1.5 pr-3 text-left font-medium">{nameOf(id)}</th>
              {GOVERNED_FIELDS.map((f) => {
                const may = rule.allowed_fields.includes(f);
                const Icon = !may ? Eye : direct ? AlertCircle : Check;
                return (
                  <td key={f} className="px-2 py-1.5">
                    <span className={`inline-flex items-center gap-1 ${may ? "text-foreground" : "text-muted-foreground"}`}>
                      <Icon aria-hidden="true" className="size-3.5" />
                      {!may
                        ? t(($) => $.delegated_writes.read_only)
                        : direct
                          ? t(($) => $.delegated_writes.writes_directly)
                          : t(($) => $.delegated_writes.may_write)}
                      {may && isNew(id, f) ? (
                        <span className="rounded-full bg-muted px-1.5 text-caption font-medium">
                          {t(($) => $.delegated_writes.new_tag)}
                        </span>
                      ) : null}
                    </span>
                  </td>
                );
              })}
            </tr>
          ))}
        </tbody>
      </table>
      <p className="mt-2 text-caption text-muted-foreground">{t(($) => $.delegated_writes.others_read_only)}</p>
    </div>
  );

  const editor = (
    <div className="space-y-4 border-t border-surface-border pt-4">
      <fieldset className="space-y-2">
        <legend className="text-body font-medium">{t(($) => $.delegated_writes.allowed_agents)}</legend>
        <div className="flex flex-wrap items-center gap-2">
          {rule.allowed_agents.map((id) => {
            const archived = !!agents.find((a) => a.id === id)?.archived_at;
            return (
              <span key={id} className="inline-flex items-center gap-1 rounded-full border border-surface-border px-2 py-0.5 text-caption">
                {nameOf(id)}
                {archived ? ` · ${t(($) => $.delegated_writes.archived)}` : ""}
                <button
                  type="button"
                  aria-label={t(($) => $.delegated_writes.remove_agent, { name: nameOf(id) })}
                  className="rounded-full outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  onClick={() => edit({ allowed_agents: rule.allowed_agents.filter((a) => a !== id) })}
                >
                  <X aria-hidden="true" className="size-3" />
                </button>
              </span>
            );
          })}
          <select
            aria-label={t(($) => $.delegated_writes.add_agent)}
            value=""
            disabled={addable.length === 0}
            className="h-7 rounded-md border border-input bg-background px-2 text-caption outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:border-dashed"
            onChange={(e) => e.target.value && edit({ allowed_agents: [...rule.allowed_agents, e.target.value] })}
          >
            <option value="">{t(($) => $.delegated_writes.add_agent)}</option>
            {addable.map((a) => (
              <option key={a.id} value={a.id}>{a.name}</option>
            ))}
          </select>
        </div>
      </fieldset>
      <fieldset className="space-y-2">
        <legend className="text-body font-medium">{t(($) => $.delegated_writes.allowed_fields)}</legend>
        <div className="grid gap-x-4 gap-y-1.5 sm:grid-cols-2">
          {GOVERNED_FIELDS.map((f) => (
            <label key={f} className="flex items-center gap-2 font-mono text-caption">
              <Checkbox
                checked={rule.allowed_fields.includes(f)}
                onCheckedChange={() => edit({ allowed_fields: toggle(rule.allowed_fields, f) })}
              />
              {f}
            </label>
          ))}
        </div>
      </fieldset>
      <div className="flex items-start justify-between gap-4">
        <div>
          <p className="text-body font-medium">{t(($) => $.delegated_writes.requires_decision)}</p>
          <p className="text-caption text-muted-foreground">
            {direct
              ? t(($) => $.delegated_writes.requires_decision_off)
              : t(($) => $.delegated_writes.requires_decision_on)}
          </p>
        </div>
        <Switch
          checked={rule.requires_human_decision}
          aria-label={t(($) => $.delegated_writes.requires_decision)}
          onCheckedChange={(on) => {
            setConfirmed(false);
            edit({ requires_human_decision: on });
          }}
        />
      </div>
      {needsConfirm ? (
        <label className="flex items-start gap-2 rounded-md border border-warning/40 p-2 text-caption">
          <Checkbox
            checked={confirmed}
            onCheckedChange={(v) => setConfirmed(v === true)}
          />
          {t(($) => $.delegated_writes.confirm_direct)}
        </label>
      ) : null}
      {error ? (
        <p role="alert" className="flex items-start gap-2 text-caption text-destructive">
          <AlertCircle aria-hidden="true" className="mt-0.5 size-3.5 shrink-0" />
          <span>
            <span className="font-medium">{t(($) => $.delegated_writes.not_saved)}</span> {error}
          </span>
        </p>
      ) : null}
      <div className="flex items-center justify-end gap-2">
        {dirty ? <span className="mr-auto text-caption text-muted-foreground">{t(($) => $.delegated_writes.unsaved)}</span> : null}
        <Button variant="outline" size="sm" onClick={cancel}>{t(($) => $.delegated_writes.cancel)}</Button>
        <Button
          size="sm"
          disabled={!dirty || saving || (needsConfirm && !confirmed)}
          className="disabled:border disabled:border-dashed disabled:border-muted-foreground/50 disabled:bg-muted disabled:text-muted-foreground disabled:opacity-100"
          onClick={() => void save()}
        >
          {t(($) => $.delegated_writes.save)}
        </Button>
      </div>
    </div>
  );

  const editButton = canEdit && !editing ? (
    <Button variant="outline" size="sm" onClick={() => setOpened(true)}>
      {empty ? t(($) => $.delegated_writes.setup) : t(($) => $.delegated_writes.edit)}
    </Button>
  ) : null;

  return shell(
    // An empty rule carries its own button in the body; the header shows Edit rule otherwise.
    empty ? null : editButton,
    <>
      {!canEdit ? (
        <p role="note" className="flex items-start gap-2 text-caption text-muted-foreground">
          <Lock aria-hidden="true" className="mt-0.5 size-3.5 shrink-0" />
          {t(($) => $.delegated_writes.member_note)}
        </p>
      ) : null}
      {empty ? (
        <div className="space-y-2">
          <p className="text-body font-medium">{t(($) => $.delegated_writes.empty_title)}</p>
          <p className="text-caption text-muted-foreground">{t(($) => $.delegated_writes.empty_body)}</p>
          {editButton}
        </div>
      ) : (
        <>
          <p className="text-body">{summary}</p>
          {grid}
        </>
      )}
      {editing ? editor : null}
    </>,
  );
}
