"use client";

import { useId, useState } from "react";
import { ApiError } from "@multica/core/api";
import { useWorkspaceId } from "@multica/core/hooks";
import { useUpdateWorkspaceJevGovernance } from "@multica/core/workspace/mutations";
import type { WorkspaceJevGovernance } from "@multica/core/types";
import { createSafeId } from "@multica/core/utils";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { useT } from "../../i18n";
import { SettingsRow } from "./settings-layout";

/** What the server uses when the workspace sets no URL. */
export const DEFAULT_JEV_API_URL = "https://api.typesafe.ai/v1";

/** The router's rule: http or https scheme and a host. Empty is valid (it clears). */
export function isValidJevApiUrl(value: string) {
  if (value === "") return true;
  if (!/^https?:\/\/[^/?#\s]\S*$/i.test(value)) return false;
  try {
    new URL(value);
    return true;
  } catch {
    return false;
  }
}

/**
 * The Jev API URL, next to the provider key. Owners and admins save it with the
 * version they read, like the routing pane; members see the value only.
 */
export function JevApiUrlRow({
  config,
  canManage,
  online,
  onNotice,
}: {
  config: WorkspaceJevGovernance;
  canManage: boolean;
  online: boolean;
  onNotice: (notice: "url_saved" | "url_cleared") => void;
}) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const update = useUpdateWorkspaceJevGovernance(wsId);
  const inputId = useId();
  const [draft, setDraft] = useState<string | null>(null);
  const [failure, setFailure] = useState<"conflict" | "failed" | null>(null);

  const text = draft ?? config.api_url;
  const trimmed = text.trim();
  const invalid = !isValidJevApiUrl(trimmed);
  const editable = online && !update.isPending;

  const submit = async (value: string) => {
    setFailure(null);
    try {
      await update.mutateAsync({ expected_version: config.version, request_id: createSafeId(), jev_api_url: value });
      setDraft(null);
      onNotice(value === "" ? "url_cleared" : "url_saved");
    } catch (error) {
      setFailure(error instanceof ApiError && error.status === 409 ? "conflict" : "failed");
    }
  };

  return (
    <SettingsRow
      label={<label htmlFor={inputId}>{t(($) => $.jev_governance.api_url.label)}</label>}
      description={t(($) => $.jev_governance.api_url.hint, { default: DEFAULT_JEV_API_URL })}
      size="text"
      align="start"
    >
      {canManage ? (
        <div className="space-y-2">
          <Input
            id={inputId}
            data-testid="jev-api-url"
            type="text"
            inputMode="url"
            autoComplete="off"
            spellCheck={false}
            placeholder={DEFAULT_JEV_API_URL}
            value={text}
            disabled={!editable}
            aria-invalid={invalid ? true : undefined}
            aria-describedby={invalid ? `${inputId}-error` : undefined}
            onChange={(event) => {
              setFailure(null);
              setDraft(event.target.value);
            }}
          />
          {invalid ? (
            <p id={`${inputId}-error`} className="text-caption text-destructive">
              {t(($) => $.jev_governance.api_url.invalid)}
            </p>
          ) : null}
          {failure ? (
            <p role="alert" className="text-caption text-destructive">
              {failure === "conflict"
                ? t(($) => $.jev_governance.api_url.conflict)
                : t(($) => $.jev_governance.api_url.save_failed)}
            </p>
          ) : null}
          <div className="flex flex-wrap items-center gap-2">
            <Button
              size="sm"
              disabled={!editable || invalid || trimmed === config.api_url}
              aria-busy={update.isPending ? true : undefined}
              onClick={() => void submit(trimmed)}
            >
              {t(($) => $.jev_governance.api_url.save)}
            </Button>
            {config.api_url ? (
              <Button variant="ghost" size="sm" disabled={!editable} onClick={() => void submit("")}>
                {t(($) => $.jev_governance.api_url.clear)}
              </Button>
            ) : null}
          </div>
        </div>
      ) : (
        <p className="break-all text-body">
          {config.api_url || t(($) => $.jev_governance.api_url.default_value, { url: DEFAULT_JEV_API_URL })}
        </p>
      )}
    </SettingsRow>
  );
}
