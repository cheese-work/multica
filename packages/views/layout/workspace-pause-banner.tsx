"use client";

import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { AlertTriangle } from "@multica/ui/icons";
import { useAuthStore } from "@multica/core/auth";
import { useCurrentWorkspace } from "@multica/core/paths";
import { useSetWorkspaceScheduling } from "@multica/core/workspace/mutations";
import { memberListOptions } from "@multica/core/workspace/queries";
import { Button } from "@multica/ui/components/ui/button";
import { useT, useTimeAgo } from "../i18n";

/**
 * Shown on every page while an owner/admin has paused claims for the whole
 * workspace (CHE-2954). Everyone sees it; only an owner or admin gets Resume.
 * The server stays the source of truth for who may resume.
 */
export function WorkspacePauseBanner() {
  const { t } = useT("layout");
  const timeAgo = useTimeAgo();
  const workspace = useCurrentWorkspace();
  const userId = useAuthStore((s) => s.user?.id);
  const paused = !!workspace?.scheduling_paused_at;
  const { data: members = [] } = useQuery({
    ...memberListOptions(workspace?.id ?? ""),
    enabled: paused,
  });
  const resume = useSetWorkspaceScheduling();
  if (!workspace || !workspace.scheduling_paused_at) return null;

  const myRole = members.find((m) => m.user_id === userId)?.role;
  const canResume = myRole === "owner" || myRole === "admin";
  const by = members.find((m) => m.user_id === workspace.scheduling_paused_by)?.name;
  const time = timeAgo(workspace.scheduling_paused_at);

  return (
    <div className="flex shrink-0 flex-col gap-2 px-3 pt-3">
      <div
        role="status"
        className="flex items-center gap-2.5 rounded-lg border border-warning/60 bg-warning/15 px-3 py-2 text-caption"
      >
        <AlertTriangle aria-hidden="true" className="size-4 shrink-0 text-warning" />
        <span className="min-w-0 flex-1">
          <strong className="font-semibold">
            {by
              ? t(($) => $.workspace_pause.paused_by, { name: by, time })
              : t(($) => $.workspace_pause.paused_at, { time })}
          </strong>{" "}
          {t(($) => $.workspace_pause.detail)}
        </span>
        {canResume && (
          <Button
            type="button"
            size="xs"
            variant="outline"
            disabled={resume.isPending}
            aria-busy={resume.isPending}
            onClick={() =>
              resume.mutate(
                { id: workspace.id, pause: false },
                { onError: () => toast.error(t(($) => $.workspace_pause.resume_failed)) },
              )
            }
          >
            {t(($) => $.workspace_pause.resume)}
          </Button>
        )}
      </div>
    </div>
  );
}
