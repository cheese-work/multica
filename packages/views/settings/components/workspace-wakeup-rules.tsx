"use client";

import { useId, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Pencil, Trash2 } from "@multica/ui/icons";
import { ApiError } from "@multica/core/api";
import {
  previewWorkspaceWakeupDefinition,
  useCreateWorkspaceWakeupDefinition,
  useDeleteWorkspaceWakeupDefinition,
  useSaveWorkspaceWakeupDefinition,
  workspaceWakeupDefinitionsOptions,
} from "@multica/core/workspace/wakeup-definitions";
import type { WakeupDefinition, WakeupDefinitionConfig, WakeupEffectiveRule } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { WakeupFields } from "../../issues/components/wakeup-fields";
import { useWakeupForm } from "../../issues/components/use-wakeup-form";
import { buildWakeupInput, type WakeupDraft } from "../../issues/components/wakeup-draft";
import {
  WAKEUP_DEFINITION_NAME_MAX,
  definitionConfigFromInput,
  draftFromDefinitionConfig,
  type WakeupDefinitionConfigError,
} from "../../issues/components/wakeup-definition-config";
import { useT } from "../../i18n";
import { SettingsCard, SettingsSection } from "./settings-layout";

class DraftRefused extends Error {
  constructor(readonly reason: WakeupDefinitionConfigError) {
    super(reason);
  }
}

/** A custom rule's key is a UUID; the platform rules keep their names. */
const CUSTOM_RULE_KEY = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/**
 * Workspace wakeup rules, edited with the same fields as an issue's wakeup. It
 * renders only when the server accepts definition writes, so an older server,
 * or one whose activation has not finished, shows nothing new.
 */
export function WorkspaceWakeupRules({ workspaceId, canManage }: { workspaceId: string; canManage: boolean }) {
  const { t } = useT("settings");
  const { data } = useQuery(workspaceWakeupDefinitionsOptions(workspaceId));
  // A saved rule empties the form: a new key remounts it.
  const [formKey, setFormKey] = useState(0);
  if (!data?.capabilities.definition_writes) return null;
  const rules = data.definitions.filter((d) => d.root && CUSTOM_RULE_KEY.test(d.rule_key));
  return (
    <SettingsSection
      anchor="rules"
      title={t(($) => $.wakeups.rules.title)}
      description={t(($) => $.wakeups.rules.description)}
    >
      <SettingsCard>
        {rules.length === 0 ? (
          <p className="px-4 py-3.5 text-caption text-muted-foreground">{t(($) => $.wakeups.rules.empty)}</p>
        ) : (
          <ul className="divide-y divide-border">
            {rules.map((rule) => (
              <RuleRow key={rule.rule_key} workspaceId={workspaceId} canManage={canManage} rule={rule} />
            ))}
          </ul>
        )}
        {canManage && <RuleForm key={formKey} workspaceId={workspaceId} onDone={() => setFormKey((k) => k + 1)} />}
      </SettingsCard>
    </SettingsSection>
  );
}

function RuleRow({
  workspaceId,
  canManage,
  rule,
}: {
  workspaceId: string;
  canManage: boolean;
  rule: WakeupDefinition;
}) {
  const { t } = useT("settings");
  const remove = useDeleteWorkspaceWakeupDefinition(workspaceId);
  // The rule as it was when the edit opened. A refetch must not move the
  // revision under a draft that was read at an older one.
  const [editing, setEditing] = useState<WakeupDefinition | null>(null);
  const name = rule.config.name ?? "";
  const editable = draftFromDefinitionConfig(rule.config);
  const pinned = editing ? draftFromDefinitionConfig(editing.config) : null;
  if (editing && pinned) {
    return (
      <li>
        <RuleForm
          workspaceId={workspaceId}
          rule={editing}
          initial={pinned}
          onDone={() => setEditing(null)}
          onCancel={() => setEditing(null)}
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
              onClick={() => setEditing(rule)}
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
  rule,
  initial,
  onDone,
  onCancel,
}: {
  workspaceId: string;
  rule?: WakeupDefinition;
  initial?: { name: string; draft: Partial<WakeupDraft> };
  onDone: () => void;
  onCancel?: () => void;
}) {
  const { t } = useT("settings");
  const id = useId();
  const create = useCreateWorkspaceWakeupDefinition(workspaceId);
  const save = useSaveWorkspaceWakeupDefinition(workspaceId);
  const [name, setName] = useState(initial?.name ?? "");
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
  const { draft, update, error, pending, submit } = useWakeupForm({
    workspaceId,
    defaultAgentId: "",
    initial: initial?.draft,
    save: async (input) => {
      const result = definitionConfigFromInput(input, name);
      if ("error" in result) throw new DraftRefused(result.error);
      if (!rule) {
        await create.mutateAsync(result.config);
        return;
      }
      await save.mutateAsync({ ruleKey: rule.rule_key, revision: rule.revision, config: configToSave(result.config) });
    },
    onSaved: onDone,
    describeError: (err) => {
      if (err instanceof DraftRefused) return refusals[err.reason];
      if (err instanceof ApiError && err.status === 409) return t(($) => $.wakeups.rules.conflict_error);
      const code = err instanceof ApiError && err.body && typeof err.body === "object" && "code" in err.body ? err.body.code : undefined;
      return code === "wakeup_definition_writes_closed" ? t(($) => $.wakeups.rules.closed_error) : undefined;
    },
  });

  const preview = async () => {
    setPreviewed(null);
    setPreviewError("");
    const built = buildWakeupInput(draft);
    const result = "input" in built ? definitionConfigFromInput(built.input, name) : null;
    if (!result || "error" in result) {
      // Only an incomplete draft gets here, so this shows what is missing
      // and saves nothing.
      await submit();
      return;
    }
    setPreviewing(true);
    try {
      setPreviewed(await previewWorkspaceWakeupDefinition(rule?.rule_key ?? crypto.randomUUID(), configToSave(result.config)));
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
        {t(($) => (rule ? $.wakeups.rules.edit_title : $.wakeups.rules.new_title))}
      </h3>
      <label htmlFor={`${id}-name`} className="mt-3 mb-1.5 block text-caption font-medium">
        {t(($) => $.wakeups.rules.name)}
      </label>
      <Input id={`${id}-name`} value={name} maxLength={WAKEUP_DEFINITION_NAME_MAX * 4} onChange={(event) => setName(event.target.value)} />
      <WakeupFields draft={draft} update={update} scope={{ workspaceId, definition: true }} assigneeAgentId={null} />
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
      {previewed && <PreviewSummary rule={previewed} />}
      <div className="mt-4 flex justify-end gap-2 border-t border-border pt-3">
        {onCancel && (
          <Button type="button" variant="ghost" size="sm" disabled={pending} onClick={onCancel}>
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

function PreviewSummary({ rule }: { rule: WakeupEffectiveRule }) {
  const { t } = useT("settings");
  const met = rule.already_satisfied;
  const caps = rule.aggregate_caps.filter((cap) => cap.scope === "workspace");
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
        <p key={`${cap.scope}:${cap.scope_id}`}>{t(($) => $.wakeups.rules.preview_cap, { limit: cap.limit })}</p>
      ))}
    </div>
  );
}
