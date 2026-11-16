"use client";

import { useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { usePersistedDraft } from "@multica/core/drafts";
import { useGuardEditDraftStore, type SettingsFieldsDraft } from "@multica/core/settings/draft-stores";
import { GuardPasskeyCancelled, GuardPolicyConflict, saveGuardPolicy } from "@multica/core/guard/save";
import { guardPolicyOptions } from "@multica/core/guard/queries";
import type { GuardPolicyVersion } from "@multica/core/api/passkey-schema";
import { workspaceKeys } from "@multica/core/workspace/queries";
import type { Workspace } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { useT } from "../../i18n";
import { type GuardEdit, type GuardRow, applyGuardEdit, parseGuardPolicy } from "./guard-model";

const EMPTY_DRAFT: SettingsFieldsDraft = {};
const EDIT_ACTIONS = ["fix", "deny", "shadow", "pass"] as const;
const short = (sha: string) => sha.slice(0, 8);

type Phase =
  | { kind: "idle" }
  | { kind: "saving" }
  | { kind: "saved"; sha: string }
  | { kind: "cancelled" }
  | { kind: "error"; message: string }
  | { kind: "conflict"; current: GuardPolicyVersion };

/** Values of the named workspace plugin in a stored policy; null when it is gone. */
function currentValues(version: GuardPolicyVersion, name: string): GuardEdit | null {
  const plugin = parseGuardPolicy({ guard_policy: version.policy })?.plugins.find((p) => p.scope === "workspace" && p.name === name);
  return plugin && plugin.action !== "unknown" ? { action: plugin.action, message: plugin.message } : null;
}

/** The policy version a draft started from, or null when the stored JSON is unusable. */
function parseBase(raw: string | undefined): GuardPolicyVersion | null {
  if (!raw) return null;
  try {
    const value = JSON.parse(raw) as Partial<GuardPolicyVersion>;
    return value && typeof value.sha256 === "string" && value.policy && typeof value.policy === "object" ? (value as GuardPolicyVersion) : null;
  } catch {
    return null;
  }
}

/**
 * Edit one workspace-level rule with a passkey. The edit names the digest of the
 * policy it started from; if another change landed first the server answers 409
 * and the Reapply panel offers to put this change on the new version. The unsent
 * edit and the version it started from live in the keyed draft store, so a refetch,
 * a plugin switch or a reload keeps both; the base only moves on Reload, Reapply
 * or a successful save, and the draft is dropped on a save or an explicit Cancel.
 */
export function GuardEditor({ row, onClose }: { row: GuardRow; onClose: () => void }) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const { data: base, isError } = useQuery(guardPolicyOptions(wsId));
  const [draft, patchDraft, clearDraft] = usePersistedDraft(useGuardEditDraftStore, `workspace:${row.name}`, EMPTY_DRAFT);
  const frozen = useMemo(() => parseBase(draft.base), [draft.base]);
  const edit: GuardEdit = {
    action: (draft.action as GuardEdit["action"] | undefined) ?? (row.action === "unknown" ? "pass" : row.action),
    message: draft.message ?? row.message,
  };
  // Every change freezes the version the form started from; the live query never replaces it.
  const change = (next: Partial<GuardEdit>) => {
    const values = { ...edit, ...next };
    patchDraft({ action: values.action, message: values.message, base: draft.base ?? (base ? JSON.stringify(base) : undefined) });
  };
  const against = frozen ?? base;
  const [phase, setPhase] = useState<Phase>({ kind: "idle" });
  const busy = phase.kind === "saving";

  const submit = async (against: GuardPolicyVersion) => {
    const policy = applyGuardEdit(against.policy, row.name, edit);
    if (!policy) {
      setPhase({ kind: "error", message: t(($) => $.guard.edit_plugin_gone) });
      return;
    }
    setPhase({ kind: "saving" });
    try {
      const saved = await saveGuardPolicy(wsId, { policy, base_sha256: against.sha256 });
      qc.setQueryData(workspaceKeys.list(), (old: Workspace[] | undefined) =>
        old?.map((ws) => (ws.id === wsId ? { ...ws, settings: { ...((ws.settings as Record<string, unknown>) ?? {}), guard_policy: saved.policy } } : ws)),
      );
      qc.setQueryData(guardPolicyOptions(wsId).queryKey, saved);
      clearDraft();
      setPhase({ kind: "saved", sha: saved.sha256 });
    } catch (error) {
      if (error instanceof GuardPolicyConflict) setPhase({ kind: "conflict", current: error.current });
      else if (error instanceof GuardPasskeyCancelled) setPhase({ kind: "cancelled" });
      else setPhase({ kind: "error", message: error instanceof Error ? error.message : String(error) });
    }
  };

  const discard = () => {
    clearDraft();
    onClose();
  };

  if (phase.kind === "conflict") {
    const { current } = phase;
    const pending = { from: row.action, to: edit.action };
    const label = (action: string) => ({ fix: t(($) => $.guard.action_fix), deny: t(($) => $.guard.action_deny), shadow: t(($) => $.guard.action_watch), pass: t(($) => $.guard.action_pass) })[action] ?? action;
    return (
      <div className="space-y-3 border-b border-surface-border bg-muted/40 p-4 text-label">
        <p role="alert" className="font-medium text-destructive">
          {t(($) => $.guard.conflict_title, { from: short(against?.sha256 ?? ""), to: short(current.sha256), name: row.name })}
        </p>
        <div className="space-y-1 rounded-md border border-surface-border bg-card p-3">
          <p className="text-micro font-semibold uppercase tracking-wide text-muted-foreground">{t(($) => $.guard.conflict_pending_title)}</p>
          <p>{t(($) => $.guard.conflict_pending, { name: row.name, from: label(pending.from), to: label(pending.to) })}</p>
          {edit.message.trim() && <p className="break-words text-muted-foreground">{t(($) => $.guard.conflict_pending_message, { message: edit.message.trim() })}</p>}
        </div>
        <div className="flex flex-wrap gap-2">
          <Button type="button" size="sm" className="min-h-11 md:min-h-0" onClick={() => void submit(current)}>
            {t(($) => $.guard.conflict_reapply, { to: short(current.sha256) })}
          </Button>
          <Button
            type="button"
            size="sm"
            variant="outline"
            className="min-h-11 md:min-h-0"
            onClick={() => {
              const values = currentValues(current, row.name);
              if (values) patchDraft({ ...values, base: JSON.stringify(current) });
              else clearDraft();
              qc.setQueryData(guardPolicyOptions(wsId).queryKey, current);
              setPhase({ kind: "idle" });
            }}
          >
            {t(($) => $.guard.conflict_reload)}
          </Button>
          <Button type="button" size="sm" variant="ghost" className="min-h-11 md:min-h-0" onClick={discard}>
            {t(($) => $.guard.conflict_discard)}
          </Button>
        </div>
      </div>
    );
  }

  return (
    <form
      className="space-y-3 border-b border-surface-border bg-muted/40 p-4 text-label"
      onSubmit={(event) => {
        event.preventDefault();
        if (against && !busy) void submit(against);
      }}
    >
      <p className="font-medium">{t(($) => $.guard.edit_title)}</p>
      <div className="grid gap-3 md:grid-cols-[140px_minmax(0,1fr)] md:items-center">
        <label htmlFor="guard-edit-action" className="text-muted-foreground">{t(($) => $.guard.action)}</label>
        <select
          id="guard-edit-action"
          value={edit.action}
          disabled={busy || !base}
          onChange={(event) => change({ action: event.target.value as GuardEdit["action"] })}
          className="h-9 min-h-11 rounded-md border border-surface-border bg-background px-2 md:min-h-0"
        >
          {EDIT_ACTIONS.map((action) => (
            <option key={action} value={action}>
              {{ fix: t(($) => $.guard.action_fix), deny: t(($) => $.guard.action_deny), shadow: t(($) => $.guard.action_watch), pass: t(($) => $.guard.action_pass) }[action]}
            </option>
          ))}
        </select>
        <label htmlFor="guard-edit-message" className="text-muted-foreground">{t(($) => $.guard.message)}</label>
        <div className="space-y-1">
          <Input id="guard-edit-message" value={edit.message} disabled={busy || !base} onChange={(event) => change({ message: event.target.value })} />
          <p className="text-caption text-muted-foreground">{t(($) => $.guard.edit_message_hint)}</p>
        </div>
      </div>
      {isError && <p role="alert" className="text-caption text-destructive">{t(($) => $.guard.edit_load_error)}</p>}
      {phase.kind === "cancelled" && <p role="status" className="text-caption text-destructive">{t(($) => $.guard.edit_cancelled)}</p>}
      {phase.kind === "error" && <p role="alert" className="text-caption text-destructive">{t(($) => $.guard.edit_failed, { reason: phase.message })}</p>}
      {phase.kind === "saving" && <p role="status" className="text-caption text-muted-foreground">{t(($) => $.guard.edit_saving)}</p>}
      {phase.kind === "saved" && <p role="status" className="text-caption text-success">{t(($) => $.guard.edit_saved, { version: short(phase.sha) })}</p>}
      <div className="flex flex-wrap gap-2">
        <Button type="submit" size="sm" className="min-h-11 md:min-h-0" disabled={!against || busy}>
          {t(($) => $.guard.edit_save)}
        </Button>
        <Button type="button" size="sm" variant="ghost" className="min-h-11 md:min-h-0" onClick={phase.kind === "saved" ? onClose : discard}>
          {phase.kind === "saved" ? t(($) => $.guard.edit_done) : t(($) => $.guard.edit_cancel)}
        </Button>
      </div>
    </form>
  );
}
