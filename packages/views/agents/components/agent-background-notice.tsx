"use client";

import type { AgentBackgroundUse } from "@multica/core/types";
import { useWorkspacePaths } from "@multica/core/paths";
import { AppLink } from "../../navigation";
import { useT } from "../../i18n";

/**
 * Frame E of design/background-agents/approved: one row per job the agent
 * serves, with scope badge, a link to the tab that edits it, and the job key.
 * Workspace rows link only for owners/admins (members cannot edit that tab).
 * The server returns just workspace uses and the viewer's own personal uses.
 */
export function AgentBackgroundUseList({
  uses,
  canEditWorkspace,
}: {
  uses: AgentBackgroundUse[];
  canEditWorkspace: boolean;
}) {
  const { t } = useT("agents");
  const paths = useWorkspacePaths();
  const hasWorkspaceUse = uses.some((u) => u.scope === "workspace");
  return (
    <div className="min-w-0">
      <span className="font-medium text-foreground">
        {t(($) => $.background_use.title)}
      </span>
      {" · "}
      {t(($) => $.background_use.intro)}
      <ul className="mt-1.5 space-y-1">
        {uses.map((u) => {
          const personal = u.scope === "personal";
          return (
            <li
              key={`${u.scope}:${u.job}`}
              className="flex flex-wrap items-center gap-x-2 gap-y-0.5"
            >
              <span className="text-foreground">
                {t(($) => $.background_use.jobs[u.job])}
              </span>
              <span className="rounded-sm bg-muted px-1.5 py-0.5 text-caption">
                {personal
                  ? t(($) => $.background_use.scope_personal)
                  : t(($) => $.background_use.scope_workspace)}
              </span>
              {(personal || canEditWorkspace) && (
                <AppLink
                  href={`${paths.settings()}?tab=${personal ? "personal-background-agents" : "background-agents"}`}
                  className="underline underline-offset-2"
                >
                  {personal
                    ? t(($) => $.background_use.link_personal)
                    : t(($) => $.background_use.link_workspace)}
                </AppLink>
              )}
              <code className="text-muted-foreground">{u.job}</code>
            </li>
          );
        })}
      </ul>
      {hasWorkspaceUse && !canEditWorkspace && (
        <p className="mt-1.5">{t(($) => $.background_use.member_note)}</p>
      )}
    </div>
  );
}
