"use client";

import { useEffect, useId, useRef, useState, useSyncExternalStore, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { Lock, WifiOff } from "lucide-react";
import { ApiError } from "@multica/core/api";
import { useWorkspaceId } from "@multica/core/hooks";
import { useCurrentMember } from "@multica/core/permissions";
import { workspaceJevGovernanceOptions } from "@multica/core/workspace/queries";
import { useUpdateWorkspaceJevGovernance } from "@multica/core/workspace/mutations";
import type { JevRoutingMode, WorkspaceJevGovernance } from "@multica/core/types";
import { createSafeId } from "@multica/core/utils";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import {
  SettingsCard,
  SettingsReadOnlyNotice,
  SettingsRow,
  SettingsSaveState,
  SettingsSection,
  SettingsTab,
  type SettingsSaveStatus,
} from "./settings-layout";

// The server falls back to this when the workspace never set a confidence.
const DEFAULT_MIN_CONFIDENCE = 0.9;
const SELECTABLE_MODES = ["off", "shadow"] as const;

function subscribeOnline(onChange: () => void) {
  window.addEventListener("online", onChange);
  window.addEventListener("offline", onChange);
  return () => {
    window.removeEventListener("online", onChange);
    window.removeEventListener("offline", onChange);
  };
}

function useOnline() {
  return useSyncExternalStore(
    subscribeOnline,
    () => navigator.onLine,
    () => true,
  );
}

/** A confidence is a number from 0 to 1; anything else is not sent. */
function parseConfidence(text: string): number | null {
  const trimmed = text.trim();
  if (trimmed === "") return null;
  const value = Number(trimmed);
  return Number.isFinite(value) && value >= 0 && value <= 1 ? value : null;
}

/** Two decimals, unless that would round the stored value: an untouched confidence is never rewritten. */
function formatConfidence(value: number | null) {
  const v = value ?? DEFAULT_MIN_CONFIDENCE;
  const fixed = v.toFixed(2);
  return Number(fixed) === v ? fixed : String(v);
}

/** What the user is editing, and the version they started from. */
interface Draft {
  baseVersion: number;
  mode: JevRoutingMode;
  confidence: string;
}

/**
 * Jev / Governance, routing pane: the squad routing mode and the minimum
 * confidence, from the workspace governance config. Members see it read-only;
 * owners and admins save with the version they read (`expected_version`), so a
 * concurrent save is a conflict they can recover from, not an overwrite.
 *
 * Design: design/jev-governance-tab/rev-4/C.html. Credential and history
 * panes of that design are later layers and are not offered here.
 */
export function JevGovernanceTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const { role } = useCurrentMember(wsId);
  const query = useQuery(workspaceJevGovernanceOptions(wsId));
  const title = t(($) => $.page.tabs.jev_governance);
  const config = query.data;

  return (
    <SettingsTab title={title} scope="workspace">
      {config ? (
        <RoutingPane
          config={config}
          canManage={role === "owner" || role === "admin"}
          reload={async () => (await query.refetch()).isSuccess}
        />
      ) : query.isError ? (
        <div
          role="alert"
          className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-destructive/30 bg-destructive/5 px-4 py-3"
        >
          <div className="min-w-0">
            <p className="text-body font-medium">{t(($) => $.jev_governance.load_error)}</p>
            <p className="text-caption text-muted-foreground">
              {t(($) => $.jev_governance.load_error_body)}
            </p>
          </div>
          <Button variant="outline" size="sm" onClick={() => void query.refetch()}>
            {t(($) => $.jev_governance.retry)}
          </Button>
        </div>
      ) : (
        <div
          role="status"
          aria-label={t(($) => $.jev_governance.loading)}
          className="space-y-3"
        >
          <Skeleton className="h-5 w-40" />
          <Skeleton className="h-28 w-full rounded-xl" />
        </div>
      )}
    </SettingsTab>
  );
}

function RoutingPane({
  config,
  canManage,
  reload,
}: {
  config: WorkspaceJevGovernance;
  canManage: boolean;
  /** Resolves true only when the latest config loaded. */
  reload: () => Promise<boolean>;
}) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const online = useOnline();
  const update = useUpdateWorkspaceJevGovernance(wsId);
  const groupId = useId();
  const confidenceId = useId();
  const reasonId = useId();
  const inputRef = useRef<HTMLInputElement>(null);

  const [draft, setDraft] = useState<Draft | null>(null);
  const [status, setStatus] = useState<SettingsSaveStatus>("idle");
  const [conflict, setConflict] = useState(false);

  const mode = draft?.mode ?? config.routing_mode;
  const confidenceText = draft?.confidence ?? formatConfidence(config.min_confidence);
  const parsedConfidence = parseConfidence(confidenceText);
  const invalid = parsedConfidence === null;
  const savedConfidence = config.min_confidence ?? DEFAULT_MIN_CONFIDENCE;
  const modeChanged = mode !== config.routing_mode;
  const confidenceChanged = !invalid && parsedConfidence !== savedConfidence;
  const dirty = draft !== null && (modeChanged || confidenceChanged || invalid);
  const editable = canManage && online && status !== "saving";

  // Focus follows the error: the invalid field, not the disabled Save.
  useEffect(() => {
    if (invalid && draft) inputRef.current?.focus();
  }, [invalid, draft]);

  const edit = (next: Partial<Draft>) => {
    setStatus("idle");
    setDraft({ baseVersion: draft?.baseVersion ?? config.version, mode, confidence: confidenceText, ...next });
  };
  const discard = () => {
    setDraft(null);
    setStatus("idle");
    setConflict(false);
  };

  const save = async () => {
    if (!draft || invalid || parsedConfidence === null) return;
    setStatus("saving");
    setConflict(false);
    try {
      await update.mutateAsync({
        expected_version: draft.baseVersion,
        request_id: createSafeId(),
        ...(modeChanged && mode !== "on" ? { jev_squad_routing_mode: mode } : {}),
        ...(confidenceChanged ? { jev_min_confidence: parsedConfidence } : {}),
      });
      // The cache now holds the confirmed config; nothing is pending any more.
      setDraft(null);
      setStatus("saved");
    } catch (error) {
      if (error instanceof ApiError && error.status === 409) {
        setStatus("idle");
        setConflict(true);
      } else {
        setStatus("error");
      }
    }
  };

  const reloadLatest = async () => {
    // A failed reload keeps the edit and the conflict recovery on screen.
    if (await reload()) discard();
  };

  const modeLabel = t(($) => $.jev_governance.mode[mode]);

  return (
    <div className="space-y-6" data-testid="jev-governance-routing">
      <div className="flex min-h-5 flex-wrap items-center gap-x-3 gap-y-1">
        <p className="text-body text-muted-foreground">
          {t(($) => $.jev_governance.summary, { version: config.version, mode: modeLabel })}
        </p>
        <SettingsSaveState
          status={status}
          savingLabel={t(($) => $.jev_governance.saving)}
          savedLabel={t(($) => $.jev_governance.saved)}
          errorLabel={t(($) => $.jev_governance.save_error)}
        />
      </div>

      {!online ? (
        <div
          role="status"
          className="flex items-start gap-3 rounded-xl border border-warning/40 bg-warning/5 px-4 py-3"
        >
          <WifiOff aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-warning" />
          <div className="min-w-0">
            <p className="text-body font-medium">{t(($) => $.jev_governance.offline_title)}</p>
            <p className="text-caption text-muted-foreground">
              {t(($) => $.jev_governance.offline_body, { version: config.version })}
            </p>
          </div>
        </div>
      ) : null}

      {!canManage ? <SettingsReadOnlyNotice wsId={wsId} /> : null}

      <SettingsSection title={t(($) => $.jev_governance.routing_title)} anchor="routing">
        <SettingsCard>
          <SettingsRow
            label={<span id={`${groupId}-label`}>{t(($) => $.jev_governance.mode_label)}</span>}
            description={t(($) => $.jev_governance.mode_hint[mode])}
            size="none"
          >
            <div
              role="radiogroup"
              data-testid="jev-routing-mode"
              aria-labelledby={`${groupId}-label`}
              className="inline-flex rounded-lg bg-muted p-0.5"
            >
              {SELECTABLE_MODES.map((value) => (
                <ModeOption
                  key={value}
                  name={groupId}
                  checked={mode === value}
                  disabled={!editable}
                  onSelect={() => edit({ mode: value })}
                >
                  {t(($) => $.jev_governance.mode[value])}
                </ModeOption>
              ))}
              <ModeOption
                name={groupId}
                checked={mode === "on"}
                disabled
                describedBy={reasonId}
                onSelect={() => undefined}
              >
                <Lock aria-hidden="true" className="size-3" />
                {t(($) => $.jev_governance.mode.on)}
              </ModeOption>
            </div>
          </SettingsRow>
          <p
            id={reasonId}
            className="flex items-center gap-2 bg-muted/40 px-4 py-2 text-caption text-muted-foreground"
          >
            <Lock aria-hidden="true" className="size-3 shrink-0" />
            {t(($) => $.jev_governance.on_locked)}
          </p>
          <SettingsRow
            label={
              <label htmlFor={confidenceId}>{t(($) => $.jev_governance.confidence_label)}</label>
            }
            description={t(($) => $.jev_governance.confidence_hint)}
            size="code"
          >
            <div className="space-y-1">
              <Input
                id={confidenceId}
                data-testid="jev-min-confidence"
                ref={inputRef}
                type="text"
                inputMode="decimal"
                value={confidenceText}
                disabled={!editable}
                aria-invalid={invalid && draft ? true : undefined}
                aria-describedby={invalid && draft ? `${confidenceId}-error` : undefined}
                onChange={(event) => edit({ confidence: event.target.value })}
              />
              {invalid && draft ? (
                <p id={`${confidenceId}-error`} className="text-caption text-destructive">
                  {t(($) => $.jev_governance.confidence_invalid)}
                </p>
              ) : null}
            </div>
          </SettingsRow>
          {canManage && dirty ? (
            <div
              data-testid="jev-save-bar"
              className="flex flex-wrap items-center justify-between gap-3 bg-muted/40 px-4 py-3"
            >
              <span className="text-caption text-muted-foreground">
                {t(($) => $.jev_governance.based_on, { version: draft?.baseVersion ?? config.version })}
              </span>
              <div className="flex items-center gap-2">
                <Button variant="outline" size="sm" onClick={discard} disabled={status === "saving"}>
                  {t(($) => $.jev_governance.cancel)}
                </Button>
                <Button
                  size="sm"
                  onClick={() => void save()}
                  disabled={invalid || !online || status === "saving"}
                  aria-busy={status === "saving" ? true : undefined}
                >
                  {t(($) => $.jev_governance.save)}
                </Button>
              </div>
            </div>
          ) : null}
        </SettingsCard>
      </SettingsSection>

      {conflict ? (
        <div
          role="alert"
          data-testid="jev-conflict-alert"
          className="space-y-3 rounded-xl border border-destructive/30 bg-destructive/5 px-4 py-3"
        >
          <div className="min-w-0">
            <p className="text-body font-medium">{t(($) => $.jev_governance.conflict_title)}</p>
            <p className="text-caption text-muted-foreground">
              {t(($) => $.jev_governance.conflict_body)}
            </p>
          </div>
          <div className="flex flex-wrap gap-2">
            <Button variant="outline" size="sm" onClick={() => void reloadLatest()}>
              {t(($) => $.jev_governance.conflict_reload)}
            </Button>
            <Button variant="ghost" size="sm" onClick={() => setConflict(false)}>
              {t(($) => $.jev_governance.conflict_keep)}
            </Button>
          </div>
        </div>
      ) : null}
    </div>
  );
}

/** One segment of the mode control: a native radio, so the keyboard comes for free. */
function ModeOption({
  name,
  checked,
  disabled,
  describedBy,
  onSelect,
  children,
}: {
  name: string;
  checked: boolean;
  disabled: boolean;
  describedBy?: string;
  onSelect: () => void;
  children: ReactNode;
}) {
  return (
    <label className="relative">
      <input
        type="radio"
        name={name}
        checked={checked}
        disabled={disabled}
        aria-describedby={describedBy}
        onChange={onSelect}
        className="peer sr-only"
      />
      <span
        className={cn(
          "inline-flex h-7 min-w-14 cursor-pointer items-center justify-center gap-1 rounded-md px-3 text-label text-muted-foreground",
          "peer-checked:bg-background peer-checked:font-medium peer-checked:text-foreground peer-checked:shadow-xs",
          "peer-focus-visible:ring-2 peer-focus-visible:ring-ring",
          "peer-disabled:cursor-not-allowed peer-disabled:opacity-60",
        )}
      >
        {children}
      </span>
    </label>
  );
}
