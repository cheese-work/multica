"use client";

import { useEffect, useId, useRef, useState } from "react";
import { usePersistedDraft } from "@multica/core/drafts";
import { useWakeupRuleDraftStore, type SettingsFieldsDraft } from "@multica/core/settings/draft-stores";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight, Pencil, Trash2 } from "@multica/ui/icons";
import { ApiError } from "@multica/core/api";
import {
  WORKSPACE_WAKEUP_SCOPE,
  previewWakeupDefinition,
  useCreateWakeupDefinition,
  useDeleteWakeupDefinition,
  useSaveWakeupDefinition,
  wakeupDefinitionsOptions,
  type EditableWakeupScope,
} from "@multica/core/workspace/wakeup-definitions";
import type { WakeupDefinition, WakeupDefinitionConfig, WakeupEffectiveRule } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { WakeupFields } from "../../issues/components/wakeup-fields";
import { useWakeupForm } from "../../issues/components/use-wakeup-form";
import type { WakeupDraft } from "../../issues/components/wakeup-draft";
import {
  WAKEUP_DEFINITION_NAME_MAX,
  definitionConfigFromInput,
  draftFromDefinitionConfig,
  type WakeupDefinitionConfigError,
} from "../../issues/components/wakeup-definition-config";
import { useT } from "../../i18n";
import { SettingsCard, SettingsSection } from "./settings-layout";

const NO_RULE_DRAFT: SettingsFieldsDraft = {};

class DraftRefused extends Error {
  constructor(readonly reason: WakeupDefinitionConfigError) {
    super(reason);
  }
}

/** A custom rule's key is a UUID; the platform rules keep their names. */
const CUSTOM_RULE_KEY = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function WorkspaceWakeupRules({ workspaceId, canManage }: { workspaceId: string; canManage: boolean }) {
  return <ScopedWakeupRules workspaceId={workspaceId} scope={WORKSPACE_WAKEUP_SCOPE} canManage={canManage} />;
}

/**
 * Custom wakeup rules of a workspace or project, edited with the same fields
 * as an issue's wakeup. It renders only when the server accepts definition
 * writes, so an older server, or one whose activation has not finished, shows
 * nothing new.
 */
export function ScopedWakeupRules({
  workspaceId,
  scope,
  canManage,
  toggleLabel,
  revealSignal = 0,
}: {
  workspaceId: string;
  scope: EditableWakeupScope;
  canManage: boolean;
  /** Each increase opens the collapsed toggle and brings it into view (a deep link). */
  revealSignal?: number;
  /** Puts the rules behind a collapsed toggle with this label; the drafts survive a collapse. */
  toggleLabel?: string;
}) {
  const { t } = useT("settings");
  const bodyId = useId();
  const [open, setOpen] = useState(!toggleLabel);
  const toggleRef = useRef<HTMLButtonElement | null>(null);
  useEffect(() => {
    if (!revealSignal) return;
    setOpen(true);
    toggleRef.current?.scrollIntoView?.({ block: "nearest" });
  }, [revealSignal]);
  const project = scope.kind === "project";
  const { data } = useQuery(wakeupDefinitionsOptions(workspaceId, scope));
  // A saved rule empties the form: a new key remounts it.
  const [formKey, setFormKey] = useState(0);
  // Rules opened for editing, as they were when the edit opened. They live here,
  // not in the row, so a rule someone else deletes mid-edit does not take the
  // open draft with it when the list refetches.
  const [editing, setEditing] = useState<Record<string, WakeupDefinition>>({});
  if (!data?.capabilities.definition_writes) return null;
  const listed = data.definitions.filter((d) => d.root && CUSTOM_RULE_KEY.test(d.rule_key));
  const gone = Object.values(editing).filter((open) => !listed.some((d) => d.rule_key === open.rule_key));
  const rules = [...listed, ...gone];
  const setEditingRule = (key: string, rule: WakeupDefinition | null) =>
    setEditing(({ [key]: _closed, ...rest }) => (rule ? { ...rest, [key]: rule } : rest));
  const section = (
    <SettingsSection
      anchor="rules"
      title={t(($) => (project ? $.wakeups.rules.project_title : $.wakeups.rules.title))}
      description={t(($) => (project ? $.wakeups.rules.project_description : $.wakeups.rules.description))}
    >
      <SettingsCard>
        {rules.length === 0 ? (
          <p className="px-4 py-3.5 text-caption text-muted-foreground">
            {t(($) => (project ? $.wakeups.rules.project_empty : $.wakeups.rules.empty))}
          </p>
        ) : (
          <ul className="divide-y divide-border">
            {rules.map((rule) => (
              <RuleRow
                key={rule.rule_key}
                workspaceId={workspaceId}
                scope={scope}
                canManage={canManage}
                rule={rule}
                editing={editing[rule.rule_key] ?? null}
                onEdit={(opened) => setEditingRule(rule.rule_key, opened)}
              />
            ))}
          </ul>
        )}
        {canManage && <RuleForm key={formKey} workspaceId={workspaceId} scope={scope} onDone={() => setFormKey((k) => k + 1)} />}
      </SettingsCard>
    </SettingsSection>
  );
  if (!toggleLabel) return section;
  return (
    <div>
      <button
        ref={toggleRef}
        type="button"
        aria-expanded={open}
        aria-controls={bodyId}
        className={`mb-2 flex w-full items-center gap-1 rounded-md px-2 py-1 text-caption font-medium transition-colors hover:bg-accent/70 ${open ? "" : "text-muted-foreground hover:text-foreground"}`}
        onClick={() => setOpen(!open)}
      >
        {toggleLabel}
        <ChevronRight className={`!size-3 shrink-0 stroke-[2.5] text-muted-foreground transition-transform ${open ? "rotate-90" : ""}`} />
      </button>
      <div id={bodyId} hidden={!open}>
        {section}
      </div>
    </div>
  );
}

function RuleRow({
  workspaceId,
  scope,
  canManage,
  rule,
  editing,
  onEdit,
}: {
  workspaceId: string;
  scope: EditableWakeupScope;
  canManage: boolean;
  rule: WakeupDefinition;
  editing: WakeupDefinition | null;
  onEdit: (opened: WakeupDefinition | null) => void;
}) {
  const { t } = useT("settings");
  const remove = useDeleteWakeupDefinition(workspaceId, scope);
  // `editing` is the rule as it was when the edit opened. A refetch must not
  // move the revision under a draft that was read at an older one.
  const name = rule.config.name ?? "";
  const editable = draftFromDefinitionConfig(rule.config);
  const pinned = editing ? draftFromDefinitionConfig(editing.config) : null;
  if (editing && pinned) {
    return (
      <li>
        <RuleForm
          workspaceId={workspaceId}
          scope={scope}
          rule={editing}
          initial={pinned}
          onDone={() => onEdit(null)}
          onCancel={() => onEdit(null)}
        />
      </li>
    );
  }
  return (
    <li className="flex items-start justify-between gap-3 px-4 py-3">
      <div className="min-w-0">
        <p className="text-body font-medium">{name}</p>
        {rule.config.instruction && (
          <p className="mt-0.5 line-clamp-2 text-caption text-muted-foreground">{rule.config.instruction}</p>
        )}
        {canManage && !editable && (
          <p className="mt-1 text-caption text-muted-foreground">{t(($) => $.wakeups.rules.edit_unsupported)}</p>
        )}
        {remove.isError && (
          <p role="alert" className="mt-1 text-caption text-destructive">
            {t(($) => $.wakeups.rules.delete_error)}
          </p>
        )}
      </div>
      {canManage && (
        <div className="flex shrink-0 gap-1">
          {editable && (
            <Button
              type="button"
              variant="ghost"
              size="icon"
              aria-label={t(($) => $.wakeups.rules.edit, { name })}
              onClick={() => onEdit(rule)}
            >
              <Pencil className="size-3.5" aria-hidden="true" />
            </Button>
          )}
          <Button
            type="button"
            variant="ghost"
            size="icon"
            aria-label={t(($) => $.wakeups.rules.delete, { name })}
            disabled={remove.isPending}
            onClick={() => remove.mutate({ ruleKey: rule.rule_key, revision: rule.revision })}
          >
            <Trash2 className="size-3.5" aria-hidden="true" />
          </Button>
        </div>
      )}
    </li>
  );
}

/** Creates a rule, or with `rule` edits that saved rule at the revision `rule` carries. */
function RuleForm({
  workspaceId,
  scope,
  rule,
  initial,
  onDone,
  onCancel,
}: {
  workspaceId: string;
  scope: EditableWakeupScope;
  rule?: WakeupDefinition;
  initial?: { name: string; draft: Partial<WakeupDraft> };
  onDone: () => void;
  onCancel?: () => void;
}) {
  const { t } = useT("settings");
  const id = useId();
  const create = useCreateWakeupDefinition(workspaceId, scope);
  const save = useSaveWakeupDefinition(workspaceId, scope);
  // Unsent input is kept per scope and rule (or the new-rule form), so it
  // survives a remount or reload; a saved rule or a cancel clears it.
  const draftKey = `${scope.kind}:${scope.kind === "project" ? scope.id : workspaceId}:${rule?.rule_key ?? "new"}`;
  const [persisted, setPersisted, clearPersisted] = usePersistedDraft(useWakeupRuleDraftStore, draftKey, NO_RULE_DRAFT);
  const [name, setNameState] = useState(persisted.name ?? initial?.name ?? "");
  // The store fills in after mount when the workspace storage namespace
  // switches; a draft arriving then applies once, unless the user typed first.
  const touched = useRef(Boolean(persisted.name ?? persisted.form));
  const setName = (next: string) => {
    touched.current = true;
    setNameState(next);
    setPersisted({ name: next });
  };
  const [previewed, setPreviewed] = useState<WakeupEffectiveRule | null>(null);
  const [previewError, setPreviewError] = useState("");
  const [previewing, setPreviewing] = useState(false);
  // Fields this form does not edit (rate limit, filters, ...) stay as saved.
  // Preview and save send the same configuration.
  const configToSave = (edited: WakeupDefinitionConfig): WakeupDefinitionConfig => {
    if (!rule) return edited;
    const merged = { ...rule.config, ...edited };
    if (edited.max_fires === undefined) delete merged.max_fires;
    return merged;
  };
  const refusals: Record<WakeupDefinitionConfigError, string> = {
    missing_name: t(($) => $.wakeups.rules.missing_name, { max: WAKEUP_DEFINITION_NAME_MAX }),
    unsupported_filter: t(($) => $.wakeups.rules.unsupported_filter),
    unsupported_schedule: t(($) => $.wakeups.rules.unsupported_schedule),
  };
  const { draft, update: updateDraft, error, pending, submit, build } = useWakeupForm({
    workspaceId,
    defaultAgentId: "",
    initial: { ...initial?.draft, ...(persisted.form ? (JSON.parse(persisted.form) as Partial<WakeupDraft>) : undefined) },
    save: async (input) => {
      const result = definitionConfigFromInput(input, name);
      if ("error" in result) throw new DraftRefused(result.error);
      if (!rule) {
        await create.mutateAsync(result.config);
        return;
      }
      await save.mutateAsync({ ruleKey: rule.rule_key, revision: rule.revision, config: configToSave(result.config) });
    },
    onSaved: () => {
      clearPersisted();
      onDone();
    },
    describeError: (err) => {
      if (err instanceof DraftRefused) return refusals[err.reason];
      if (err instanceof ApiError && err.status === 409) return t(($) => $.wakeups.rules.conflict_error);
      const code = err instanceof ApiError && err.body && typeof err.body === "object" && "code" in err.body ? err.body.code : undefined;
      return code === "wakeup_definition_writes_closed" ? t(($) => $.wakeups.rules.closed_error) : undefined;
    },
  });

  useEffect(() => {
    if (touched.current || !(persisted.name ?? persisted.form)) return;
    touched.current = true;
    if (persisted.name !== undefined) setNameState(persisted.name);
    if (persisted.form) updateDraft(JSON.parse(persisted.form) as Partial<WakeupDraft>);
  }, [persisted, updateDraft]);

  const update = (patch: Partial<WakeupDraft>) => {
    touched.current = true;
    updateDraft(patch);
    setPersisted({ form: JSON.stringify({ ...draft, ...patch }) });
  };

  const preview = async () => {
    setPreviewed(null);
    setPreviewError("");
    const built = build();
    const result = "input" in built ? definitionConfigFromInput(built.input, name) : null;
    if (!result || "error" in result) {
      // Only an incomplete draft gets here, so this shows what is missing
      // and saves nothing.
      await submit();
      return;
    }
    setPreviewing(true);
    try {
      setPreviewed(await previewWakeupDefinition(scope, rule?.rule_key ?? crypto.randomUUID(), configToSave(result.config)));
    } catch {
      setPreviewError(t(($) => $.wakeups.rules.preview_error));
    } finally {
      setPreviewing(false);
    }
  };

  return (
    <form
      aria-labelledby={`${id}-title`}
      className="px-4 py-3.5"
      onSubmit={(event) => {
        event.preventDefault();
        void submit();
      }}
    >
      <h3 id={`${id}-title`} className="text-body font-medium">
        {t(($) =>
          scope.kind === "project"
            ? rule ? $.wakeups.rules.project_edit_title : $.wakeups.rules.project_new_title
            : rule ? $.wakeups.rules.edit_title : $.wakeups.rules.new_title,
        )}
      </h3>
      <label htmlFor={`${id}-name`} className="mt-3 mb-1.5 block text-caption font-medium">
        {t(($) => $.wakeups.rules.name)}
      </label>
      <Input id={`${id}-name`} value={name} disabled={pending} maxLength={WAKEUP_DEFINITION_NAME_MAX * 4} onChange={(event) => setName(event.target.value)} />
      <fieldset disabled={pending} className="contents">
        <WakeupFields draft={draft} update={update} scope={{ workspaceId, definition: true }} assigneeAgentId={null} />
      </fieldset>
      {error && (
        <p role="alert" className="mt-3 text-caption text-destructive">
          {error}
        </p>
      )}
      {previewError && (
        <p role="alert" className="mt-3 text-caption text-destructive">
          {previewError}
        </p>
      )}
      {previewed && <PreviewSummary rule={previewed} scope={scope} />}
      <div className="mt-4 flex justify-end gap-2 border-t border-border pt-3">
        {onCancel && (
          <Button type="button" variant="ghost" size="sm" disabled={pending} onClick={() => {
              clearPersisted();
              onCancel();
            }}>
            {t(($) => $.wakeups.rules.cancel)}
          </Button>
        )}
        <Button type="button" variant="outline" size="sm" disabled={pending || previewing} onClick={() => void preview()}>
          {t(($) => (previewing ? $.wakeups.rules.previewing : $.wakeups.rules.preview))}
        </Button>
        <Button type="submit" size="sm" disabled={pending} aria-busy={pending}>
          {t(($) =>
            rule
              ? pending ? $.wakeups.rules.saving : $.wakeups.rules.save
              : pending ? $.wakeups.rules.creating : $.wakeups.rules.create,
          )}
        </Button>
      </div>
    </form>
  );
}

function PreviewSummary({ rule, scope }: { rule: WakeupEffectiveRule; scope: EditableWakeupScope }) {
  const { t } = useT("settings");
  const met = rule.already_satisfied;
  const caps = rule.aggregate_caps.filter((cap) => cap.scope === scope.kind);
  return (
    <div role="status" className="mt-3 space-y-1 rounded-md bg-muted/60 px-3 py-2 text-caption leading-5 text-muted-foreground">
      {met ? (
        <p>
          {t(($) => (met.truncated ? $.wakeups.rules.preview_met_truncated : $.wakeups.rules.preview_met), {
            satisfied: met.satisfied,
            examined: met.examined,
          })}
        </p>
      ) : (
        <p>{t(($) => $.wakeups.rules.preview_ok)}</p>
      )}
      {caps.map((cap) => (
        <p key={`${cap.scope}:${cap.scope_id}`}>
          {t(($) => (scope.kind === "project" ? $.wakeups.rules.project_preview_cap : $.wakeups.rules.preview_cap), { limit: cap.limit })}
        </p>
      ))}
    </div>
  );
}
