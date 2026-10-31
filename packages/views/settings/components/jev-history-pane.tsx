"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Lock, RotateCcw } from "@multica/ui/icons";
import { useWorkspaceId } from "@multica/core/hooks";
import { memberListOptions } from "@multica/core/workspace/queries";
import type { GovernanceAuditEntry, WorkspaceJevGovernance } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { formatInTimeZone } from "../../common/format-in-time-zone";
import { SettingsSection } from "./settings-layout";
import { diffSettings, restoresRoutingOn } from "./jev-history";
import { useChangeFormat } from "./use-change-format";
import { JevRollbackDialog } from "./jev-rollback-dialog";

// The approved design prints history on the workspace's clock, labelled ICT.
const HISTORY_TIME_ZONE = "Asia/Bangkok";
// The server returns at most this many versions.
const HISTORY_LIMIT = 50;

export function JevHistoryPane({
  config,
  canManage,
  online,
  reload,
  onNotice,
}: {
  config: WorkspaceJevGovernance;
  canManage: boolean;
  online: boolean;
  /** Resolves true only when the latest config loaded. */
  reload: () => Promise<boolean>;
  onNotice: (version: number) => void;
}) {
  const { t, i18n } = useT("settings");
  const wsId = useWorkspaceId();
  const { data: members = [] } = useQuery({ ...memberListOptions(wsId), enabled: !!wsId });
  const { chip } = useChangeFormat();
  const [selected, setSelected] = useState<number | null>(null);
  const [rollbackTo, setRollbackTo] = useState<GovernanceAuditEntry | null>(null);

  const history = config.history;
  const open = history.find((entry) => entry.version === selected) ?? null;
  const who = (entry: GovernanceAuditEntry) =>
    members.find((member) => member.user_id === entry.actor_id)?.name ||
    t(($) => $.jev_governance.history.unknown_actor);
  const when = (entry: GovernanceAuditEntry) =>
    t(($) => $.jev_governance.history.when, {
      time: formatInTimeZone(entry.changed_at, HISTORY_TIME_ZONE, i18n.language),
    });

  if (history.length === 0) {
    return (
      <SettingsSection anchor="history">
        <div className="rounded-xl border border-surface-border px-4 py-8 text-center">
          <p className="text-body font-medium">{t(($) => $.jev_governance.history.empty_title)}</p>
          <p className="mt-1 text-caption text-muted-foreground">
            {t(($) => $.jev_governance.history.empty_body)}
          </p>
        </div>
      </SettingsSection>
    );
  }

  return (
    <SettingsSection
      description={`${t(($) => $.jev_governance.history.intro)} ${t(($) => $.jev_governance.history.select_hint)}`}
      anchor="history"
    >
      <ul
        data-testid="jev-history-list"
        className="divide-y divide-surface-border overflow-hidden rounded-xl border border-surface-border"
      >
        {history.map((entry) => {
          const diff = diffSettings(entry.previous, entry.effective);
          const isSelected = entry.version === selected;
          return (
            <li key={entry.version}>
              <button
                type="button"
                aria-pressed={isSelected}
                onClick={() => setSelected(isSelected ? null : entry.version)}
                className={cn(
                  "flex w-full min-w-0 flex-col gap-1.5 px-4 py-3 text-left transition-colors hover:bg-surface-hover focus-visible:outline-2 focus-visible:outline-ring",
                  isSelected && "bg-surface-selected",
                )}
              >
                <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
                  <span className="text-body font-semibold">
                    {t(($) => $.jev_governance.history.version_label, { version: entry.version })}
                  </span>{" "}
                  {entry.version === config.version ? (
                    <span className="rounded-full bg-muted px-2 py-0.5 text-caption text-muted-foreground">
                      {t(($) => $.jev_governance.history.current)}
                    </span>
                  ) : null}
                  <span className="text-caption text-muted-foreground">
                    {t(($) => $.jev_governance.history.by_when, { name: who(entry), when: when(entry) })}
                  </span>
                </span>
                <span className="flex flex-wrap gap-1.5">
                  {diff.changes.map((change) => (
                    <span
                      key={change.key}
                      className="rounded-md bg-muted px-1.5 py-0.5 text-caption text-foreground"
                    >
                      {chip(change)}
                    </span>
                  ))}
                </span>
              </button>
            </li>
          );
        })}
      </ul>
      {history.length >= HISTORY_LIMIT ? (
        <p className="text-caption text-muted-foreground">{t(($) => $.jev_governance.history.latest_limit)}</p>
      ) : null}

      {open ? (
        <section
          aria-label={t(($) => $.jev_governance.history.detail_label, { version: open.version })}
          className="space-y-3 rounded-xl border border-surface-border px-4 py-3"
        >
          <p className="text-body font-medium">
            {t(($) => $.jev_governance.history.detail_label, { version: open.version })}
          </p>
          <p className="text-caption text-muted-foreground">
            {t(($) => $.jev_governance.history.detail_by, { name: who(open), when: when(open) })}
          </p>
          {open.version === config.version ? (
            <p className="text-caption text-muted-foreground">
              {t(($) => $.jev_governance.history.current_detail)}
            </p>
          ) : canManage ? (
            <div className="space-y-2">
              <Button
                variant="outline"
                size="sm"
                disabled={!online || restoresRoutingOn(open)}
                onClick={() => setRollbackTo(open)}
              >
                <RotateCcw aria-hidden="true" />
                {t(($) => $.jev_governance.history.roll_back, { version: open.version })}
              </Button>
              {restoresRoutingOn(open) ? (
                <p className="flex items-center gap-1.5 text-caption text-muted-foreground">
                  <Lock aria-hidden="true" className="size-3 shrink-0" />
                  {t(($) => $.jev_governance.history.locked_on)}
                </p>
              ) : null}
            </div>
          ) : null}
        </section>
      ) : null}

      <JevRollbackDialog
        config={config}
        target={rollbackTo}
        online={online}
        reload={reload}
        onClose={() => setRollbackTo(null)}
        onDone={(version) => {
          setRollbackTo(null);
          setSelected(null);
          onNotice(version);
        }}
      />
    </SettingsSection>
  );
}
