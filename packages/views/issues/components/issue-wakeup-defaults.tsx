"use client";

import { useEffect, useId, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ApiError } from "@multica/core/api";
import {
  effectiveWakeupRuleOptions,
  useDeleteWakeupDefinition,
  useSaveWakeupDefinition,
  wakeupDefinitionsOptions,
} from "@multica/core/workspace/wakeup-definitions";
import type { WakeupDefinition, WakeupDefinitionConfig, WakeupDefinitionScope, WakeupEffectiveRule } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { Switch } from "@multica/ui/components/ui/switch";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { useWorkspacePaths } from "@multica/core/paths";
import { useT } from "../../i18n";
import { AppLink } from "../../navigation";
import { useWakeupText } from "./wakeup-presentation";

const MAX_INSTRUCTION_BYTES = 4000;
/** The platform rules an issue with a linked PR runs; `child_done` has its own row above. */
const PLATFORM_RULES = ["pr_merged", "pr_checks_failed", "pr_needs_attention"] as const;

/**
 * The scoped wakeup rules an issue inherits from its project and workspace, and
 * what this issue overrides. It renders only when the server accepts definition
 * writes, so an older server shows nothing new.
 */
export function IssueWakeupDefaults({ workspaceId, issueId, projectId }: { workspaceId: string; issueId: string; projectId?: string }) {
  const { t } = useT("issues");
  const scope: WakeupDefinitionScope = { kind: "issue", id: issueId };
  const { data } = useQuery(wakeupDefinitionsOptions(workspaceId, scope));
  if (!data?.capabilities.definition_writes) return null;
  const overrides = data.definitions;
  const keys = [
    ...PLATFORM_RULES.map(String),
    ...overrides.map((d) => d.rule_key).filter((key) => !(PLATFORM_RULES as readonly string[]).includes(key)),
  ];
  return (
    <div className="border-t border-border pt-2">
      <p className="px-2 pb-1 text-caption font-medium text-muted-foreground">{t(($) => $.wakeups.defaults.title)}</p>
      <ul aria-label={t(($) => $.wakeups.defaults.title)} className="space-y-0.5">
        {keys.map((key) => (
          <DefaultRule
            key={key}
            workspaceId={workspaceId}
            scope={scope}
            ruleKey={key}
            projectId={projectId}
            override={overrides.find((d) => d.rule_key === key) ?? null}
          />
        ))}
      </ul>
    </div>
  );
}

function DefaultRule({
  workspaceId,
  scope,
  ruleKey,
  override,
  projectId,
}: {
  workspaceId: string;
  scope: WakeupDefinitionScope;
  ruleKey: string;
  override: WakeupDefinition | null;
  projectId?: string;
}) {
  const { t } = useT("issues");
  const paths = useWorkspacePaths();
  const text = useWakeupText();
  const remove = useDeleteWakeupDefinition(workspaceId, scope);
  // The override as it was when the edit opened: a refetch must not move the
  // revision under a draft that was read at an older one.
  const [editing, setEditing] = useState<{ revision: string; base: WakeupDefinitionConfig } | null>(null);
  const { data: rule } = useQuery(effectiveWakeupRuleOptions(workspaceId, scope, ruleKey));
  if (!rule?.applicable) return null;
  const title = ruleTitle(t, ruleKey, rule);
  const pausedReason = rule.execution?.paused_reason;
  const state = pausedReason ? "paused" : rule.enabled ? "on" : "disabled";
  const stateText = {
    on: t(($) => $.wakeups.defaults.state_on),
    disabled: t(($) => $.wakeups.defaults.state_disabled),
    paused: t(($) => $.wakeups.defaults.state_paused, {
      reason: text.pausedReason({ paused_reason: pausedReason as never, max_fires: null, fire_count: 0 }) ?? pausedReason ?? "",
    }),
  }[state];
  const origin = override
    ? t(($) => $.wakeups.defaults.overridden)
    : t(($) => $.wakeups.defaults.inherited, { source: sourceName(t, rule) });
  const open = () => setEditing({ revision: override?.revision ?? "0", base: override?.config ?? { v: 1 } });
  return (
    <li aria-label={title} className="rounded-md px-2 py-1.5 text-label">
      <div className="flex min-w-0 items-center gap-2">
        <span className="min-w-0 flex-1 break-words">{title}</span>
        <span className={`shrink-0 text-caption ${state === "on" ? "text-muted-foreground" : "text-warning"}`}>{stateText}</span>
      </div>
      <p className="text-caption text-muted-foreground">
        {origin}
        {!override && projectId && inheritedFrom(rule) === "project" && (
          <>
            {" · "}
            <AppLink href={paths.projectWakeups(projectId)} className="underline underline-offset-2 hover:text-foreground">
              {t(($) => $.wakeups.defaults.open_project)}
            </AppLink>
          </>
        )}
      </p>
      {editing ? (
        <OverrideForm
          workspaceId={workspaceId}
          scope={scope}
          ruleKey={ruleKey}
          rule={rule}
          editing={editing}
          onClose={() => setEditing(null)}
        />
      ) : (
        <>
          {rule.config.instruction && (
            <p className="mt-0.5 line-clamp-2 break-words text-caption text-muted-foreground">{rule.config.instruction}</p>
          )}
          {!rule.redacted && (
            <div className="mt-1 flex gap-1">
              <Button
                type="button"
                variant="outline"
                size="sm"
                aria-label={t(($) => (override ? $.wakeups.defaults.edit_label : $.wakeups.defaults.override_label), { rule: title })}
                onClick={open}
              >
                {t(($) => (override ? $.wakeups.defaults.edit : $.wakeups.defaults.override))}
              </Button>
              {override && (
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  aria-label={t(($) => $.wakeups.defaults.reset_label, { rule: title })}
                  disabled={remove.isPending}
                  onClick={() => remove.mutate({ ruleKey, revision: override.revision })}
                >
                  {t(($) => $.wakeups.defaults.reset)}
                </Button>
              )}
            </div>
          )}
          {remove.isError && (
            <p role="alert" className="mt-1 text-caption text-destructive">
              {t(($) => $.wakeups.defaults.reset_error)}
            </p>
          )}
        </>
      )}
    </li>
  );
}

function OverrideForm({
  workspaceId,
  scope,
  ruleKey,
  rule,
  editing,
  onClose,
}: {
  workspaceId: string;
  scope: WakeupDefinitionScope;
  ruleKey: string;
  rule: WakeupEffectiveRule;
  editing: { revision: string; base: WakeupDefinitionConfig };
  onClose: () => void;
}) {
  const { t } = useT("issues");
  const id = useId();
  const save = useSaveWakeupDefinition(workspaceId, scope);
  const resolvedInstruction = rule.config.instruction ?? "";
  const [enabled, setEnabled] = useState(rule.enabled);
  const [instruction, setInstruction] = useState(resolvedInstruction);
  const [error, setError] = useState("");
  const field = useRef<HTMLTextAreaElement>(null);
  // The button that opened the form is gone; keep the keyboard where the person was.
  useEffect(() => field.current?.focus(), []);
  const tooLong = new TextEncoder().encode(instruction).length > MAX_INSTRUCTION_BYTES;
  // Only what the person changed goes into the override; everything else keeps inheriting.
  const config: WakeupDefinitionConfig = { ...editing.base };
  if (enabled !== rule.enabled) config.enabled = enabled;
  if (instruction.trim() && instruction !== resolvedInstruction) config.instruction = instruction;
  const changed = JSON.stringify(config) !== JSON.stringify(editing.base);
  const submit = async () => {
    setError("");
    try {
      await save.mutateAsync({ ruleKey, revision: editing.revision, config });
      onClose();
    } catch (err) {
      setError(err instanceof ApiError && err.status === 409 ? t(($) => $.wakeups.defaults.conflict_error) : t(($) => $.wakeups.defaults.save_error));
    }
  };
  return (
    <form
      className="mt-1.5 space-y-2"
      onSubmit={(event) => {
        event.preventDefault();
        void submit();
      }}
    >
      <label htmlFor={`${id}-instruction`} className="block text-caption font-medium">
        {t(($) => $.wakeups.system.instruction)}
      </label>
      <Textarea
        id={`${id}-instruction`}
        ref={field}
        rows={3}
        value={instruction}
        aria-invalid={tooLong}
        className="max-h-[30dvh] resize-y text-base md:text-label"
        onChange={(event) => setInstruction(event.target.value)}
      />
      {tooLong && <p className="text-caption text-destructive">{t(($) => $.wakeups.system.instruction_invalid)}</p>}
      <div className="flex items-center gap-2">
        <span id={`${id}-enabled`} className="flex-1 text-caption">
          {t(($) => $.wakeups.system.enable)}
        </span>
        <Switch size="sm" checked={enabled} aria-labelledby={`${id}-enabled`} onCheckedChange={setEnabled} />
      </div>
      {error && (
        <p role="alert" className="text-caption text-destructive">
          {error}
        </p>
      )}
      <div className="flex justify-end gap-2">
        <Button type="submit" size="sm" disabled={!changed || tooLong || save.isPending} aria-busy={save.isPending}>
          {t(($) => (save.isPending ? $.wakeups.defaults.saving : $.wakeups.defaults.save))}
        </Button>
        <Button type="button" variant="ghost" size="sm" disabled={save.isPending} onClick={onClose}>
          {t(($) => $.wakeups.defaults.cancel)}
        </Button>
      </div>
    </form>
  );
}

type Translate = ReturnType<typeof useT<"issues">>["t"];

function ruleTitle(t: Translate, ruleKey: string, rule: WakeupEffectiveRule) {
  switch (ruleKey) {
    case "pr_merged":
      return t(($) => $.wakeups.defaults.rule_pr_merged);
    case "pr_checks_failed":
      return t(($) => $.wakeups.defaults.rule_pr_checks_failed);
    case "pr_needs_attention":
      return t(($) => $.wakeups.defaults.rule_pr_needs_attention);
    default:
      return rule.config.name || ruleKey;
  }
}

/** The scope a rule's on/off, or failing that its instruction, is inherited from. */
function inheritedFrom(rule: WakeupEffectiveRule) {
  return rule.sources.enabled ?? rule.sources.instruction;
}

function sourceName(t: Translate, rule: WakeupEffectiveRule) {
  const source = inheritedFrom(rule);
  return source === "workspace"
    ? t(($) => $.wakeups.defaults.source_workspace)
    : source === "project"
      ? t(($) => $.wakeups.defaults.source_project)
      : t(($) => $.wakeups.defaults.source_default);
}
