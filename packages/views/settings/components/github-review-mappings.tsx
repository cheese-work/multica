"use client";

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Trash2 } from "@multica/ui/icons";
import { api } from "@multica/core/api";
import { deriveGitHubReviewMappings } from "@multica/core/github";
import { useWorkspaceId } from "@multica/core/hooks";
import { useCurrentWorkspace } from "@multica/core/paths";
import type { Workspace } from "@multica/core/types";
import { agentListOptions, memberListOptions, workspaceKeys } from "@multica/core/workspace/queries";
import { Button } from "@multica/ui/components/ui/button";
import { IconButton } from "@multica/ui/components/common/icon-button";
import { Input } from "@multica/ui/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger } from "@multica/ui/components/ui/select";
import { useT } from "../../i18n";
import { SettingsCard, SettingsRow, SettingsSection } from "./settings-layout";

type MappingField = "github_review_members" | "github_review_authors";
type Option = { value: string; label: string };

/**
 * One editable key → option map (CHE-2269). Every change sends the whole map:
 * the server replaces that settings key and validates each entry.
 */
function MappingRow({
  field,
  anchor,
  label,
  description,
  keyLabel,
  keyPlaceholder,
  valueLabel,
  entries,
  options,
  canEdit,
  normalizeKey,
}: {
  field: MappingField;
  anchor: string;
  label: string;
  description: string;
  keyLabel: string;
  keyPlaceholder: string;
  valueLabel: string;
  entries: Record<string, string>;
  options: Option[];
  canEdit: boolean;
  normalizeKey: (raw: string) => string;
}) {
  const { t } = useT("settings");
  const workspace = useCurrentWorkspace();
  const qc = useQueryClient();
  const [draftKey, setDraftKey] = useState("");
  const [draftValue, setDraftValue] = useState("");
  const [saving, setSaving] = useState(false);
  const nameOf = (value: string) => options.find((o) => o.value === value)?.label ?? value;

  async function save(next: Record<string, string>) {
    if (!workspace || saving) return false;
    setSaving(true);
    try {
      const updated = await api.updateWorkspace(workspace.id, { [field]: next });
      qc.setQueryData(workspaceKeys.list(), (old: Workspace[] | undefined) =>
        old?.map((ws) => (ws.id === updated.id ? updated : ws)),
      );
      return true;
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.github_review.toast_failed));
      return false;
    } finally {
      setSaving(false);
    }
  }

  const key = normalizeKey(draftKey);
  return (
    <SettingsRow anchor={anchor} label={label} description={description} align="start" className="flex-col">
      <div className="w-full space-y-2">
        {Object.entries(entries).map(([entryKey, value]) => (
          <div key={entryKey} className="flex items-center justify-between gap-2 text-body">
            <span className="min-w-0 truncate">
              <span className="font-mono">{entryKey}</span> → <span>{nameOf(value)}</span>
            </span>
            {canEdit ? (
              <IconButton
                variant="ghost"
                size="icon-sm"
                label={t(($) => $.github_review.remove)}
                disabled={saving}
                onClick={() => save(Object.fromEntries(Object.entries(entries).filter(([k]) => k !== entryKey)))}
              >
                <Trash2 />
              </IconButton>
            ) : null}
          </div>
        ))}
        {canEdit ? (
          <form
            className="flex flex-wrap items-center gap-2"
            onSubmit={async (event) => {
              event.preventDefault();
              if (!key || !draftValue) return;
              if (await save({ ...entries, [key]: draftValue })) {
                setDraftKey("");
                setDraftValue("");
              }
            }}
          >
            <Input
              aria-label={keyLabel}
              placeholder={keyPlaceholder}
              value={draftKey}
              onChange={(event) => setDraftKey(event.target.value)}
              className="w-48"
            />
            <Select
              items={options}
              value={draftValue}
              onValueChange={(value) => value && setDraftValue(value)}
            >
              <SelectTrigger aria-label={valueLabel} className="w-48">
                <span className="truncate">{draftValue ? nameOf(draftValue) : valueLabel}</span>
              </SelectTrigger>
              <SelectContent>
                {options.map((o) => (
                  <SelectItem key={o.value} value={o.value}>
                    {o.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Button type="submit" variant="outline" size="sm" disabled={saving || !key || !draftValue}>
              {t(($) => $.github_review.add)}
            </Button>
          </form>
        ) : null}
      </div>
    </SettingsRow>
  );
}

/**
 * Who a GitHub review counts as (CHE-2269): maps reviewer GitHub IDs to
 * workspace members and pins pull requests to the agent that authored them.
 */
export function GitHubReviewMappings({ canEdit }: { canEdit: boolean }) {
  const { t } = useT("settings");
  const workspace = useCurrentWorkspace();
  const wsId = useWorkspaceId();
  const { data: members = [] } = useQuery({ ...memberListOptions(wsId), enabled: !!wsId });
  const { data: agents = [] } = useQuery({ ...agentListOptions(wsId), enabled: !!wsId });
  const { members: memberMap, authors: authorMap } = deriveGitHubReviewMappings(workspace);

  return (
    <SettingsSection title={t(($) => $.github_review.section_title)} anchor="github-review">
      <SettingsCard>
        <MappingRow
          field="github_review_members"
          anchor="github-review-members"
          label={t(($) => $.github_review.members_label)}
          description={t(($) => $.github_review.members_description)}
          keyLabel={t(($) => $.github_review.members_key)}
          keyPlaceholder={t(($) => $.github_review.members_key)}
          valueLabel={t(($) => $.github_review.members_value)}
          entries={memberMap}
          options={members.map((m) => ({ value: m.user_id, label: m.name }))}
          canEdit={canEdit}
          normalizeKey={(raw) => raw.trim()}
        />
        <MappingRow
          field="github_review_authors"
          anchor="github-review-authors"
          label={t(($) => $.github_review.authors_label)}
          description={t(($) => $.github_review.authors_description)}
          keyLabel={t(($) => $.github_review.authors_key)}
          keyPlaceholder="owner/repo#123"
          valueLabel={t(($) => $.github_review.authors_value)}
          entries={authorMap}
          options={agents.filter((a) => !a.archived_at).map((a) => ({ value: a.id, label: a.name }))}
          canEdit={canEdit}
          normalizeKey={(raw) => raw.trim().toLowerCase()}
        />
      </SettingsCard>
    </SettingsSection>
  );
}
