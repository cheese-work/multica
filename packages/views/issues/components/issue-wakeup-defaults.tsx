"use client";

import { useEffect, useId, useRef, useState, type KeyboardEvent } from "react";
import { useQueries, useQuery } from "@tanstack/react-query";
import { ApiError } from "@multica/core/api";
import {
  effectiveWakeupRuleOptions,
  useDeleteWakeupDefinition,
  useSaveWakeupDefinition,
  wakeupDefinitionsOptions,
} from "@multica/core/workspace/wakeup-definitions";
import type { WakeupDefinition, WakeupDefinitionConfig, WakeupDefinitionScope, WakeupEffectiveRule } from "@multica/core/types";
import { ChevronDown, MoreHorizontal, Pause, Pencil } from "@multica/ui/icons";
import { Button } from "@multica/ui/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@multica/ui/components/ui/dropdown-menu";
import { Switch } from "@multica/ui/components/ui/switch";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { cn } from "@multica/ui/lib/utils";
import { useWorkspacePaths } from "@multica/core/paths";
import { useT } from "../../i18n";
import { AppLink } from "../../navigation";
import { useWakeupText } from "./wakeup-presentation";

const MAX_INSTRUCTION_BYTES = 4000;
/** The platform rules an issue with a linked PR runs; `child_done` has its own row above. */
const PLATFORM_RULES = ["pr_merged", "pr_checks_failed", "pr_needs_attention"] as const;
/** Marks the button and the panel: Esc closes the panel from either, and from nowhere else. */
const DEFAULTS_MARK = "data-wakeup-defaults";

/**
 * The scoped wakeup rules an issue inherits from its project and workspace, and
 * what this issue overrides, as a header button and an inline panel the section
 * places itself. `available` is false until the server accepts definition
 * writes, so an older server shows nothing new.
 */
export function useWakeupDefaults({ workspaceId, issueId, projectId }: { workspaceId: string; issueId: string; projectId?: string }) {
  const { t } = useT("issues");
  const scope: WakeupDefinitionScope = { kind: "issue", id: issueId };
  const { data } = useQuery(wakeupDefinitionsOptions(workspaceId, scope));
  const available = !!data?.capabilities.definition_writes;
  const overrides = data?.definitions ?? [];
  const keys = available
    ? [...PLATFORM_RULES.map(String), ...overrides.map((d) => d.rule_key).filter((key) => !(PLATFORM_RULES as readonly string[]).includes(key))]
    : [];
  const rules = useQueries({ queries: keys.map((key) => effectiveWakeupRuleOptions(workspaceId, scope, key)) });
  const [open, setOpen] = useState(false);
  const panelId = useId();
  const button = useRef<HTMLButtonElement>(null);
  if (!available) return { available, button: null, panel: null, onKeyDown: undefined };

  const applicable = rules.flatMap((query, i) => (query.data?.applicable ? [{ key: keys[i]!, rule: query.data }] : []));
  const paused = applicable.filter(({ rule }) => rule.execution?.paused_reason).length;
  const on = applicable.filter(({ rule }) => rule.enabled && !rule.execution?.paused_reason).length;
  const edited = applicable.filter(({ key }) => overrides.some((d) => d.rule_key === key)).length;
  const label = paused
    ? t(($) => (edited ? $.wakeups.defaults.button_paused_edited : $.wakeups.defaults.button_paused), { n: paused, edited })
    : t(($) => (edited ? $.wakeups.defaults.button_on_edited : $.wakeups.defaults.button_on), { n: on, edited });
  const close = () => {
    setOpen(false);
    button.current?.focus();
  };
  return {
    available,
    button: (
      <Button
        ref={button}
        type="button"
        variant="ghost"
        size="sm"
        {...{ [DEFAULTS_MARK]: "" }}
        aria-expanded={open}
        aria-controls={panelId}
        onClick={() => setOpen(!open)}
        className={cn(
          "h-auto min-h-10 min-w-0 shrink justify-start gap-1 px-2 text-caption md:min-h-8",
          paused ? "text-warning" : "text-muted-foreground",
        )}
      >
        {paused > 0 && <Pause className="size-3.5 shrink-0" aria-hidden="true" />}
        {edited > 0 && <Pencil className="size-3.5 shrink-0" aria-hidden="true" />}
        <span className="truncate">{label}</span>
        <ChevronDown className={cn("size-3 shrink-0", open && "rotate-180")} aria-hidden="true" />
      </Button>
    ),
    // Closed, not unmounted: an override draft survives a close and reopen.
    panel: (
      <div id={panelId} hidden={!open} {...{ [DEFAULTS_MARK]: "" }} className="mb-1">
        <p className="px-2 pb-1 text-caption text-muted-foreground">{t(($) => $.wakeups.defaults.caption)}</p>
        <ul aria-label={t(($) => $.wakeups.defaults.title)}>
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
    ),
    // An open menu keeps its own Esc. The panel is hidden, not unmounted, so a draft in the form survives the close.
    onKeyDown: (event: KeyboardEvent<HTMLElement>) => {
      if (event.key !== "Escape" || !open) return;
      const target = event.target as HTMLElement;
      if (target.closest(`[${DEFAULTS_MARK}]`) && !target.closest("[role='menu']")) close();
    },
  };
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
  const more = useRef<HTMLButtonElement>(null);
  const wasEditing = useRef(false);
  // The menu button was gone while the form was open; keep the keyboard where the person was.
  useEffect(() => {
    if (wasEditing.current && !editing) more.current?.focus();
    wasEditing.current = !!editing;
  }, [editing]);
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
  const source = inheritedFrom(rule);
  const tag = override
    ? t(($) => $.wakeups.defaults.tag_edited)
    : source === "workspace"
      ? t(($) => $.wakeups.defaults.tag_workspace)
      : source === "project"
        ? t(($) => $.wakeups.defaults.tag_project)
        : null;
  const open = () => setEditing({ revision: override?.revision ?? "0", base: override?.config ?? { v: 1 } });
  return (
    <li aria-label={title} className="rounded-md px-2 text-label">
      <div className="flex min-h-10 min-w-0 items-center gap-2 md:min-h-8">
        <span className="min-w-0 flex-1 break-words">{title}</span>
        {tag && (
          <span className="flex shrink-0 items-center gap-0.5 text-caption text-muted-foreground">
            {override && <Pencil className="size-3" aria-hidden="true" />}
            {tag}
          </span>
        )}
        <span className={`flex shrink-0 items-center gap-0.5 text-caption ${state === "on" ? "text-muted-foreground" : "text-warning"}`}>
          {state === "paused" && <Pause className="size-3" aria-hidden="true" />}
          {stateText}
        </span>
        {!rule.redacted && !editing && (
          <DropdownMenu>
            <DropdownMenuTrigger
              render={
                <Button
                  ref={more}
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  className="size-10 md:size-7"
                  aria-label={t(($) => $.wakeups.defaults.more_label, { rule: title })}
                />
              }
            >
              <MoreHorizontal aria-hidden="true" />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem onClick={open}>{t(($) => (override ? $.wakeups.defaults.edit : $.wakeups.defaults.override))}</DropdownMenuItem>
              <DropdownMenuItem
                disabled={!override || remove.isPending}
                onClick={() => override && remove.mutate({ ruleKey, revision: override.revision })}
              >
                {t(($) => $.wakeups.defaults.reset)}
              </DropdownMenuItem>
              {!override && projectId && source === "project" && (
                <DropdownMenuItem render={<AppLink href={paths.projectWakeups(projectId)} />}>
                  {t(($) => $.wakeups.defaults.open_project)}
                </DropdownMenuItem>
              )}
            </DropdownMenuContent>
          </DropdownMenu>
        )}
      </div>
      {editing && (
        <OverrideForm
          workspaceId={workspaceId}
          scope={scope}
          ruleKey={ruleKey}
          rule={rule}
          editing={editing}
          onClose={() => setEditing(null)}
        />
      )}
      {remove.isError && (
        <p role="alert" className="pb-1 text-caption text-destructive">
          {t(($) => $.wakeups.defaults.reset_error)}
        </p>
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
