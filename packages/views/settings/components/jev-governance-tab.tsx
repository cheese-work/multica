"use client";

import { useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { Check, Lock, Minus, WifiOff } from "@multica/ui/icons";
import { ApiError } from "@multica/core/api";
import { useWorkspaceId } from "@multica/core/hooks";
import { useCurrentMember } from "@multica/core/permissions";
import { workspaceJevCredentialOptions, workspaceJevGovernanceOptions } from "@multica/core/workspace/queries";
import { useUpdateWorkspaceJevGovernance } from "@multica/core/workspace/mutations";
import type { JevRoutingMode, WorkspaceJevGovernance } from "@multica/core/types";
import { createSafeId } from "@multica/core/utils";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@multica/ui/components/ui/tabs";
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
import { DEFAULT_MIN_CONFIDENCE, formatConfidence } from "./jev-history";
import { JevCredentialPane } from "./jev-credential-pane";
import { JevHistoryPane } from "./jev-history-pane";
import { useOnline } from "./use-online";

const SELECTABLE_MODES = ["off", "shadow"] as const;

/** A confidence is a number from 0 to 1; anything else is not sent. */
function parseConfidence(text: string): number | null {
  const trimmed = text.trim();
  if (trimmed === "") return null;
  const value = Number(trimmed);
  return Number.isFinite(value) && value >= 0 && value <= 1 ? value : null;
}

/** What the user is editing, and the version they started from. */
interface Draft {
  baseVersion: number;
  mode: JevRoutingMode;
  confidence: string;
}

const TAB_KEYS = ["ArrowRight", "ArrowLeft", "Home", "End"];

/**
 * Left and Right move between the tabs, Home and End jump (the model in
 * DESIGN.md). The primitive moves focus a frame after the key; this moves it in
 * the same event, so what the screen reader announces and what is focused never
 * disagree, and the primitive's own handler never sees these keys.
 */
function moveTabFocus(event: KeyboardEvent<HTMLElement>) {
  if (!TAB_KEYS.includes(event.key)) return;
  const tabs = Array.from(
    event.currentTarget.closest<HTMLElement>('[role="tablist"]')?.querySelectorAll<HTMLElement>('[role="tab"]') ?? [],
  );
  const at = tabs.indexOf(event.currentTarget);
  if (at < 0) return;
  const next =
    event.key === "Home"
      ? 0
      : event.key === "End"
        ? tabs.length - 1
        : (at + (event.key === "ArrowRight" ? 1 : -1) + tabs.length) % tabs.length;
  event.preventDefault();
  event.stopPropagation();
  tabs[next]?.focus();
}

type Notice = { kind: "key_saved" | "key_removed" | "url_saved" | "url_cleared" } | { kind: "rolled_back"; version: number };

/**
 * Jev / Governance: Routing, Credential and History as one tab group, from the
 * workspace governance config. Members see it read-only; owners and admins save
 * with the version they read (`expected_version`), so a concurrent save is a
 * conflict they can recover from, not an overwrite.
 *
 * Design: design/jev-governance-tab/rev-4/C.html. The routing pane stays
 * mounted while another tab is open, so a pending edit survives a look at the
 * history or the key.
 */
export function JevGovernanceTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const { role } = useCurrentMember(wsId);
  const online = useOnline();
  const query = useQuery(workspaceJevGovernanceOptions(wsId));
  const credential = useQuery(workspaceJevCredentialOptions(wsId));
  const [notice, setNotice] = useState<Notice | null>(null);
  const title = t(($) => $.page.tabs.jev_governance);
  const config = query.data;
  const canManage = role === "owner" || role === "admin";
  const reload = async () => (await query.refetch()).isSuccess;

  return (
    <SettingsTab title={title} scope="workspace">
      {config ? (
        <div className="space-y-6">
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

          {notice ? (
            <p role="status" className="flex items-center gap-1.5 text-caption text-muted-foreground">
              <Check aria-hidden="true" className="size-3 text-success" />
              {notice.kind === "rolled_back"
                ? t(($) => $.jev_governance.history.done, { version: notice.version })
                : notice.kind === "key_saved"
                  ? t(($) => $.jev_governance.credential.saved)
                  : notice.kind === "key_removed"
                    ? t(($) => $.jev_governance.credential.removed)
                    : notice.kind === "url_saved"
                      ? t(($) => $.jev_governance.api_url.saved)
                      : t(($) => $.jev_governance.api_url.cleared)}
            </p>
          ) : null}

          <Tabs defaultValue="routing" onValueChange={() => setNotice(null)}>
            <TabsList
              aria-label={t(($) => $.jev_governance.tabs_label)}
              data-testid="jev-governance-tabs"
              className="h-auto sm:h-8"
            >
              <TabsTrigger value="routing" className="h-11 px-3 sm:h-auto" onKeyDown={moveTabFocus}>
                {t(($) => $.jev_governance.tab_routing)}
              </TabsTrigger>
              <TabsTrigger value="credential" className="h-11 px-3 sm:h-auto" onKeyDown={moveTabFocus}>
                {t(($) => $.jev_governance.tab_credential)}
                {credential.data ? (
                  <>
                    {credential.data.present ? (
                      <Check aria-hidden="true" className="size-3" />
                    ) : (
                      <Minus aria-hidden="true" className="size-3" />
                    )}
                    <span className="sr-only">
                      {" "}
                      (
                      {credential.data.present
                        ? t(($) => $.jev_governance.key_set)
                        : t(($) => $.jev_governance.key_not_set)}
                      )
                    </span>
                  </>
                ) : null}
              </TabsTrigger>
              <TabsTrigger value="history" className="h-11 px-3 sm:h-auto" onKeyDown={moveTabFocus}>
                {t(($) => $.jev_governance.tab_history, { count: config.history.length })}
              </TabsTrigger>
            </TabsList>
            <TabsContent value="routing" keepMounted className="pt-4">
              <RoutingPane config={config} canManage={canManage} online={online} reload={reload} />
            </TabsContent>
            <TabsContent value="credential" className="pt-4">
              <JevCredentialPane
                credential={credential}
                config={config}
                canManage={canManage}
                online={online}
                onNotice={(kind) => setNotice({ kind })}
              />
            </TabsContent>
            <TabsContent value="history" className="pt-4">
              <JevHistoryPane
                config={config}
                canManage={canManage}
                online={online}
                reload={reload}
                onNotice={(version) => setNotice({ kind: "rolled_back", version })}
              />
            </TabsContent>
          </Tabs>
        </div>
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
  online,
  reload,
}: {
  config: WorkspaceJevGovernance;
  canManage: boolean;
  online: boolean;
  /** Resolves true only when the latest config loaded. */
  reload: () => Promise<boolean>;
}) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
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
