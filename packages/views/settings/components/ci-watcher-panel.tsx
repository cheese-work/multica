"use client";

import { useId, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { clientErrorMessage } from "@multica/core/api";
import {
  CI_WATCH_EVENTS,
  CI_WATCH_MODES,
  CI_WATCH_PROTOCOL_MODES,
  ciWatchConfigOptions,
  configToForm,
  formErrors,
  formToPatch,
  isCIWatcherDisabled,
  useUpdateCIWatchConfig,
  type CIWatchConfig,
  type CIWatchForm,
} from "@multica/core/ci-watch";
import { usePersistedDraft } from "@multica/core/drafts";
import { useCurrentMember } from "@multica/core/permissions";
import { useCIWatchDraftStore, type SettingsFieldsDraft } from "@multica/core/settings/draft-stores";
import { agentListOptions, memberListOptions, squadListOptions } from "@multica/core/workspace/queries";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { useT } from "../../i18n";
import { CIWatcherEventsTable } from "./ci-watcher-events-table";
import { SettingsCard, SettingsRow } from "./settings-layout";

const EMPTY_DRAFT: SettingsFieldsDraft = {};
const SELECT_CLASS =
  "h-9 w-full rounded-lg border border-input bg-background px-3 text-body text-foreground focus-visible:outline-2 focus-visible:outline-ring";
const P_KEYS = ["p1", "p2", "p3", "p4"] as const;

/** Unsent edits live in the keyed draft store; the form is the saved config with them laid over it. */
function useCIWatchForm(repo: string, saved: CIWatchConfig) {
  const [draft, setDraft, clearDraft] = usePersistedDraft(useCIWatchDraftStore, repo, EMPTY_DRAFT);
  const base = configToForm(saved);
  const form: CIWatchForm = draft.form ? { ...base, ...(JSON.parse(draft.form) as Partial<CIWatchForm>) } : base;
  const patch = formToPatch(saved, form);
  const dirty = Object.keys(patch).length > 0;
  const change = (part: Partial<CIWatchForm>) => {
    const next = { ...form, ...part };
    if (Object.keys(formToPatch(saved, next)).length === 0) clearDraft();
    else setDraft({ form: JSON.stringify(next) });
  };
  return { form, patch, dirty, change, clearDraft };
}

function Field({ label, error, children }: { label: string; error?: string; children: (id: string) => ReactNode }) {
  const id = useId();
  return (
    <SettingsRow label={<label htmlFor={id}>{label}</label>} size="text" align="start">
      <div className="space-y-1">
        {children(id)}
        {error ? <p className="text-caption text-destructive">{error}</p> : null}
      </div>
    </SettingsRow>
  );
}

function CIWatcherForm({ wsId, repo, saved }: { wsId: string; repo: string; saved: CIWatchConfig }) {
  const { t } = useT("settings");
  const { form, patch, dirty, change, clearDraft } = useCIWatchForm(repo, saved);
  const save = useUpdateCIWatchConfig(wsId, repo);
  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const { data: squads = [] } = useQuery(squadListOptions(wsId));
  const errors = formErrors(form);
  const capError = t(($) => $.ci_watch.cap_error);
  const listError = t(($) => $.ci_watch.list_error);
  const modeLabel = (m: string) =>
    m === "off" ? t(($) => $.ci_watch.mode_off) : m === "act" ? t(($) => $.ci_watch.mode_act) : t(($) => $.ci_watch.mode_shadow);
  const pLabel = (m: string) => (m === "act" ? t(($) => $.ci_watch.mode_act_short) : t(($) => $.ci_watch.mode_shadow_short));

  const onSave = () => {
    save.mutate(patch, { onSuccess: () => clearDraft() });
  };
  const toggleEvent = (name: string, on: boolean) =>
    change({ events: on ? [...form.events, name] : form.events.filter((e) => e !== name) });
  const text = (key: "branches" | "workflowsInclude" | "workflowsExclude", label: string) => (
    <Field label={label} error={errors.includes(key) ? listError : undefined}>
      {(id) => (
        <Textarea id={id} rows={3} value={form[key]} onChange={(e) => change({ [key]: e.target.value })} />
      )}
    </Field>
  );
  const cap = (key: "maxReruns" | "maxIssues" | "maxWakes", label: string) => (
    <Field label={label} error={errors.includes(key) ? capError : undefined}>
      {(id) => (
        <Input id={id} type="number" min={0} max={100000} value={form[key]} onChange={(e) => change({ [key]: e.target.value })} />
      )}
    </Field>
  );

  return (
    <>
      <SettingsCard>
        <Field label={t(($) => $.ci_watch.mode)}>
          {(id) => (
            <select id={id} className={SELECT_CLASS} value={form.mode} onChange={(e) => change({ mode: e.target.value })}>
              {CI_WATCH_MODES.map((m) => (
                <option key={m} value={m}>{modeLabel(m)}</option>
              ))}
            </select>
          )}
        </Field>
        {text("branches", t(($) => $.ci_watch.branches))}
        <SettingsRow label={t(($) => $.ci_watch.events)} size="text" align="start">
          <fieldset className="space-y-1.5">
            {CI_WATCH_EVENTS.map((name) => (
              <label key={name} className="flex items-center gap-2 text-body">
                <input type="checkbox" checked={form.events.includes(name)} onChange={(e) => toggleEvent(name, e.target.checked)} />
                {name}
              </label>
            ))}
            <p className="text-caption text-muted-foreground">{t(($) => $.ci_watch.event_merge_group)}</p>
          </fieldset>
        </SettingsRow>
        {text("workflowsInclude", t(($) => $.ci_watch.workflows_include))}
        {text("workflowsExclude", t(($) => $.ci_watch.workflows_exclude))}
        {P_KEYS.map((k, i) => (
          <Field key={k} label={t(($) => $.ci_watch.protocol_mode, { n: i + 1 })}>
            {(id) => (
              <select id={id} className={SELECT_CLASS} value={form[k]} onChange={(e) => change({ [k]: e.target.value })}>
                {CI_WATCH_PROTOCOL_MODES.map((m) => (
                  <option key={m} value={m}>{pLabel(m)}</option>
                ))}
              </select>
            )}
          </Field>
        ))}
        <p className="px-4 py-2 text-caption text-muted-foreground">{t(($) => $.ci_watch.unknown_note)}</p>
        <Field label={t(($) => $.ci_watch.fix_owner)}>
          {(id) => (
            <select id={id} className={SELECT_CLASS} value={form.fixOwner} onChange={(e) => change({ fixOwner: e.target.value })}>
              <option value="">{t(($) => $.ci_watch.fix_owner_none)}</option>
              <optgroup label={t(($) => $.ci_watch.fix_owner_squads)}>
                {squads.map((s) => (
                  <option key={s.id} value={`squad:${s.id}`}>{s.name}</option>
                ))}
              </optgroup>
              <optgroup label={t(($) => $.ci_watch.fix_owner_agents)}>
                {agents.filter((a) => !a.archived_at).map((a) => (
                  <option key={a.id} value={`agent:${a.id}`}>{a.name}</option>
                ))}
              </optgroup>
            </select>
          )}
        </Field>
        <Field label={t(($) => $.ci_watch.notify_member)}>
          {(id) => (
            <select id={id} className={SELECT_CLASS} value={form.notifyMember} onChange={(e) => change({ notifyMember: e.target.value })}>
              <option value="">{t(($) => $.ci_watch.notify_member_none)}</option>
              {members.map((m) => (
                <option key={m.user_id} value={m.user_id}>{m.name}</option>
              ))}
            </select>
          )}
        </Field>
        {cap("maxReruns", t(($) => $.ci_watch.max_reruns))}
        {cap("maxIssues", t(($) => $.ci_watch.max_issues))}
        {cap("maxWakes", t(($) => $.ci_watch.max_wakes))}
        <div className="flex flex-wrap items-center gap-3 px-4 py-3">
          <Button onClick={onSave} disabled={!dirty || errors.length > 0 || save.isPending}>
            {save.isPending ? t(($) => $.ci_watch.saving) : t(($) => $.ci_watch.save)}
          </Button>
          <Button variant="ghost" onClick={clearDraft} disabled={!dirty}>
            {t(($) => $.ci_watch.discard)}
          </Button>
          {dirty ? <span className="text-caption text-muted-foreground">{t(($) => $.ci_watch.unsaved)}</span> : null}
          {save.isError ? (
            <p role="alert" className="text-caption text-destructive">
              {clientErrorMessage(save.error) ?? t(($) => $.ci_watch.save_error)}
            </p>
          ) : null}
        </div>
      </SettingsCard>
      <SettingsCard>
        <CIWatcherEventsTable wsId={wsId} repo={repo} shadow={saved.mode === "shadow"} />
      </SettingsCard>
    </>
  );
}

function CIWatcherEditor({ wsId, repo }: { wsId: string; repo: string }) {
  const { t } = useT("settings");
  const { data, error, isPending } = useQuery(ciWatchConfigOptions(wsId, repo));
  if (isPending) return null;
  if (!data) {
    return isCIWatcherDisabled(error) ? (
      <p role="status" className="text-caption text-muted-foreground">{t(($) => $.ci_watch.disabled)}</p>
    ) : (
      <p role="alert" className="text-caption text-destructive">{t(($) => $.ci_watch.load_error)}</p>
    );
  }
  return <CIWatcherForm wsId={wsId} repo={repo} saved={data} />;
}

/** CI watcher settings for one GitHub repo (`owner/name`). Owners and admins only. */
export function CIWatcherPanel({ wsId, repo }: { wsId: string; repo: string }) {
  const { t } = useT("settings");
  const { role } = useCurrentMember(wsId);
  const manager = role === "owner" || role === "admin";
  return (
    <section aria-label={t(($) => $.ci_watch.title)} className="space-y-3">
      {manager ? (
        <CIWatcherEditor wsId={wsId} repo={repo} />
      ) : (
        <p role="note" className="text-caption text-muted-foreground">{t(($) => $.ci_watch.manager_only)}</p>
      )}
    </section>
  );
}
