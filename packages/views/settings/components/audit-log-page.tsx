"use client";

import { useT } from "../../i18n";
import { SettingsTab } from "./settings-layout";

/** STUB (CHE-5653): renders an empty list. The tests drive the real page. */
export function AuditLogPage() {
  const { t } = useT("settings");
  return (
    <SettingsTab
      title={t(($) => $.page.tabs.audit_log)}
      description={t(($) => $.audit_log.description)}
    >
      <p className="text-body text-muted-foreground">{t(($) => $.audit_log.empty)}</p>
    </SettingsTab>
  );
}
