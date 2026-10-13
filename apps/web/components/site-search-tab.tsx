"use client";

import { useEffect, useState } from "react";
import { useWorkspacePaths } from "@multica/core/paths";
import { Button } from "@multica/ui/components/ui/button";
import { copyText } from "@multica/ui/lib/clipboard";
import { useT } from "@multica/views/i18n";
import { useNavigation } from "@multica/views/navigation";
import { SettingsCard, SettingsRow, SettingsTab } from "@multica/views/settings";

export function SiteSearchTab() {
  const { t } = useT("settings");
  const workspacePaths = useWorkspacePaths();
  const { getShareableUrl } = useNavigation();
  const [template, setTemplate] = useState("");
  const [copyStatus, setCopyStatus] = useState<"copied" | "error" | null>(null);
  const searchPath = workspacePaths.search();

  useEffect(() => {
    setTemplate(`${getShareableUrl(searchPath)}?q=%s`);
  }, [getShareableUrl, searchPath]);

  async function copyTemplate() {
    setCopyStatus((await copyText(template)) ? "copied" : "error");
  }

  return (
    <SettingsTab title={t(($) => $.site_search.title)} scope="device">
      <SettingsCard>
        <SettingsRow
          label={t(($) => $.site_search.url)}
          description={t(($) => $.site_search.hint)}
          size="none"
        >
          <div className="flex min-w-0 items-center gap-2">
            <code className="min-w-0 break-all text-caption">{template}</code>
            <Button variant="outline" size="sm" onClick={copyTemplate} disabled={!template}>
              {t(($) => $.site_search.copy)}
            </Button>
          </div>
          <p role="status" className={copyStatus === "error" ? "text-caption text-destructive" : "text-caption text-muted-foreground"}>
            {copyStatus === "copied" ? t(($) => $.site_search.copied) : copyStatus === "error" ? t(($) => $.site_search.copy_failed) : null}
          </p>
        </SettingsRow>
      </SettingsCard>
    </SettingsTab>
  );
}
