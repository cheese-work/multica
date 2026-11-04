"use client";

import { useWorkspaceId } from "@multica/core/hooks";
import { ScopedWakeupRules } from "../../settings/components/workspace-wakeup-rules";
import { useT } from "../../i18n";

/** A project's own wakeup rules, in the project sidebar, behind a collapsed toggle. */
export function ProjectWakeupsSection({ projectId, canManage }: { projectId: string; canManage: boolean }) {
  const { t } = useT("projects");
  const wsId = useWorkspaceId();
  return (
    <ScopedWakeupRules
      workspaceId={wsId}
      scope={{ kind: "project", id: projectId }}
      canManage={canManage}
      toggleLabel={t(($) => $.detail.section_wakeups)}
    />
  );
}
