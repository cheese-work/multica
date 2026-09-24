"use client";

import { useEffect, useState } from "react";
import { AlertCircle, ShieldAlert } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { useAuthStore } from "@multica/core/auth";
import { ApiError } from "@multica/core/api";
import { useCurrentWorkspace } from "@multica/core/paths";
import {
  memberListOptions,
  useUpdateWorkspaceExportPrivacy,
  workspaceExportPrivacyOptions,
} from "@multica/core/workspace";
import {
  Alert,
  AlertDescription,
  AlertTitle,
} from "@multica/ui/components/ui/alert";
import { Input } from "@multica/ui/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { useT } from "../../i18n";
import {
  SettingsCard,
  SettingsRow,
  SettingsSaveState,
  SettingsSection,
  SettingsTab,
} from "./settings-layout";

const IMPLEMENTED_REDACTION_MODE = "small";
const RETENTION_MIN_DAYS = 1;
const RETENTION_MAX_DAYS = 3650;

/**
 * CHE-766 workspace-admin dashboard controls for the CHE-755 export privacy
 * policy: redaction mode and audit-manifest retention. Owner/admin only,
 * mirroring the server's ACL (server/internal/handler/workspace_export_privacy.go)
 * — a non-admin never issues the request in the first place (see
 * `enabled` on the query below), so no member or agent can even discover
 * whether the kill switch is on by watching this panel's network calls.
 *
 * "strict" is intentionally never rendered as a selectable option: the
 * backend accepts only "small" today (see ExportRedactionModeStrict in the
 * handler) and rejects anything else with 400. Offering it here would let an
 * admin believe they had turned on protection that does not exist yet.
 */
export function ExportPrivacyTab() {
  const { t } = useT("settings");
  const user = useAuthStore((s) => s.user);
  const workspace = useCurrentWorkspace();
  const wsId = workspace?.id ?? "";

  const { data: members = [], isFetched: membersFetched } = useQuery({
    ...memberListOptions(wsId),
    enabled: !!wsId,
  });
  const currentMember = members.find((m) => m.user_id === user?.id) ?? null;
  const canManage =
    currentMember?.role === "owner" || currentMember?.role === "admin";

  const privacyQuery = useQuery(
    workspaceExportPrivacyOptions(wsId, membersFetched && canManage),
  );
  const updatePrivacy = useUpdateWorkspaceExportPrivacy(wsId);

  const [retentionInput, setRetentionInput] = useState("");
  const [retentionError, setRetentionError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  // Reset the retention draft whenever the confirmed server value changes —
  // covers both first load and a successful save landing a new number.
  useEffect(() => {
    if (privacyQuery.data) {
      setRetentionInput(String(privacyQuery.data.manifest_retention_days));
      setRetentionError(null);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- keyed on the confirmed value only
  }, [privacyQuery.data?.manifest_retention_days]);

  if (!workspace || !membersFetched) return null;

  // Non-admins never see this panel at all — matches the server's ACL, and
  // avoids implying export privacy is something a member could influence.
  if (!canManage) return null;

  const isUnavailable =
    privacyQuery.isError &&
    privacyQuery.error instanceof ApiError &&
    privacyQuery.error.status === 503;
  const isForbidden =
    privacyQuery.isError &&
    privacyQuery.error instanceof ApiError &&
    privacyQuery.error.status === 403;

  const reportSaveError = (error: unknown, fallback: string) => {
    if (error instanceof ApiError && error.status === 503) {
      setActionError(t(($) => $.export_privacy.errors.unavailable));
      return;
    }
    if (error instanceof ApiError && error.status === 403) {
      setActionError(t(($) => $.export_privacy.errors.permission_changed));
      return;
    }
    if (error instanceof ApiError && error.status === 400) {
      setActionError(error.message || fallback);
      return;
    }
    setActionError(error instanceof Error ? error.message : fallback);
  };

  const parseRetention = (raw: string): number | null => {
    if (!/^\d+$/.test(raw.trim())) return null;
    const value = Number(raw.trim());
    if (value < RETENTION_MIN_DAYS || value > RETENTION_MAX_DAYS) return null;
    return value;
  };

  const handleRetentionBlur = async () => {
    if (!privacyQuery.data) return;
    const trimmed = retentionInput.trim();
    const parsed = parseRetention(trimmed);
    if (parsed === null) {
      setRetentionError(
        t(($) => $.export_privacy.retention_range_error, {
          min: RETENTION_MIN_DAYS,
          max: RETENTION_MAX_DAYS,
        }),
      );
      return;
    }
    setRetentionError(null);
    if (parsed === privacyQuery.data.manifest_retention_days) return;
    setActionError(null);
    try {
      await updatePrivacy.mutateAsync({ manifest_retention_days: parsed });
    } catch (error) {
      // Roll the input back to the last confirmed value — a failed save must
      // not leave an admin believing an unconfirmed number is in effect.
      setRetentionInput(String(privacyQuery.data.manifest_retention_days));
      reportSaveError(error, t(($) => $.export_privacy.errors.save_failed));
    }
  };

  if (isForbidden) {
    // A role change mid-session (demoted after this panel already mounted)
    // — not the ordinary "no admins can see this" case, which already
    // returned null above.
    return (
      <SettingsTab title={t(($) => $.export_privacy.title)}>
        <Alert variant="destructive">
          <ShieldAlert />
          <AlertTitle>{t(($) => $.export_privacy.errors.permission_changed_title)}</AlertTitle>
          <AlertDescription>
            {t(($) => $.export_privacy.errors.permission_changed)}
          </AlertDescription>
        </Alert>
      </SettingsTab>
    );
  }

  if (isUnavailable) {
    return (
      <SettingsTab title={t(($) => $.export_privacy.title)}>
        <Alert>
          <AlertCircle />
          <AlertTitle>{t(($) => $.export_privacy.unavailable_title)}</AlertTitle>
          <AlertDescription>
            {t(($) => $.export_privacy.unavailable_description)}
          </AlertDescription>
        </Alert>
      </SettingsTab>
    );
  }

  if (privacyQuery.isPending) {
    return (
      <SettingsTab title={t(($) => $.export_privacy.title)}>
        <SettingsCard>
          <div
            className="space-y-4 p-4 motion-reduce:[&_[data-slot=skeleton]]:animate-none"
            aria-label={t(($) => $.export_privacy.loading)}
          >
            <Skeleton className="h-5 w-40" />
            <Skeleton className="h-4 w-full" />
          </div>
        </SettingsCard>
      </SettingsTab>
    );
  }

  if (privacyQuery.isError || !privacyQuery.data) {
    return (
      <SettingsTab title={t(($) => $.export_privacy.title)}>
        <Alert variant="destructive">
          <AlertCircle />
          <AlertTitle>{t(($) => $.export_privacy.load_failed_title)}</AlertTitle>
          <AlertDescription>
            <p>{t(($) => $.export_privacy.load_failed_description)}</p>
          </AlertDescription>
        </Alert>
      </SettingsTab>
    );
  }

  const policy = privacyQuery.data;
  const strictSelected = policy.redaction_mode !== IMPLEMENTED_REDACTION_MODE;

  return (
    <SettingsTab
      title={t(($) => $.export_privacy.title)}
      description={t(($) => $.export_privacy.description)}
    >
      <SettingsSection
        title={t(($) => $.export_privacy.section_title)}
        action={
          <SettingsSaveState
            status={updatePrivacy.isPending ? "saving" : "idle"}
            savingLabel={t(($) => $.auto_save.saving)}
            savedLabel={t(($) => $.auto_save.saved)}
            errorLabel={t(($) => $.auto_save.failed)}
          />
        }
      >
        <SettingsCard>
          <SettingsRow
            label={t(($) => $.export_privacy.mode_label)}
            description={t(($) => $.export_privacy.mode_hint)}
            size="text"
          >
            {/* Only "small" is ever a selectable item: the trigger can show an
                unrecognized stored value (e.g. a future "strict" row written
                by a later release) read-only, but the dropdown never offers
                it, so an admin can't select a mode nothing enforces yet. */}
            <Select
              items={[
                {
                  value: IMPLEMENTED_REDACTION_MODE,
                  label: t(($) => $.export_privacy.mode_small_label),
                },
              ]}
              value={IMPLEMENTED_REDACTION_MODE}
              disabled={strictSelected || updatePrivacy.isPending}
            >
              <SelectTrigger aria-label={t(($) => $.export_privacy.mode_label)}>
                <SelectValue>
                  {() =>
                    strictSelected
                      ? policy.redaction_mode
                      : t(($) => $.export_privacy.mode_small_label)
                  }
                </SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={IMPLEMENTED_REDACTION_MODE}>
                  {t(($) => $.export_privacy.mode_small_label)}
                </SelectItem>
              </SelectContent>
            </Select>
          </SettingsRow>

          <SettingsRow
            label={t(($) => $.export_privacy.retention_label)}
            description={t(($) => $.export_privacy.retention_hint, {
              min: RETENTION_MIN_DAYS,
              max: RETENTION_MAX_DAYS,
            })}
            size="text"
          >
            <Input
              type="number"
              inputMode="numeric"
              min={RETENTION_MIN_DAYS}
              max={RETENTION_MAX_DAYS}
              step={1}
              aria-label={t(($) => $.export_privacy.retention_label)}
              aria-invalid={!!retentionError}
              value={retentionInput}
              disabled={updatePrivacy.isPending}
              onChange={(event) => {
                setRetentionError(null);
                setActionError(null);
                setRetentionInput(event.currentTarget.value);
              }}
              onBlur={handleRetentionBlur}
            />
          </SettingsRow>
          {retentionError && (
            <div className="px-4 pb-3 text-caption text-destructive">
              {retentionError}
            </div>
          )}
          {actionError && (
            <div className="px-4 pb-3 text-caption text-destructive">
              {actionError}
            </div>
          )}
        </SettingsCard>
      </SettingsSection>
    </SettingsTab>
  );
}
