import { useState, useEffect, useCallback, useRef } from "react";
import {
  Loader2,
  CheckCircle2,
  XCircle,
  ArrowUpCircle,
  Check,
  Lock,
} from "@multica/ui/icons";
import { Button } from "@multica/ui/components/ui/button";
import { api } from "@multica/core/api";
import type { RuntimeUpdateInfo, RuntimeUpdateStatus } from "@multica/core/types";
import { useT } from "../../i18n";

const statusConfig: Record<
  RuntimeUpdateStatus,
  { icon: typeof Loader2; color: string }
> = {
  pending: { icon: Loader2, color: "text-muted-foreground" },
  running: { icon: Loader2, color: "text-info" },
  completed: { icon: CheckCircle2, color: "text-success" },
  failed: { icon: XCircle, color: "text-destructive" },
  timeout: { icon: XCircle, color: "text-warning" },
};

interface UpdateSectionProps {
  /** Null for a read-only viewer who cannot use a runtime as the command channel. */
  runtimeId: string | null;
  currentVersion: string | null;
  /**
   * The server's commit-based verdict for this machine's daemon (CHE-2641).
   * The server compares the daemon's build commit with the build it carries;
   * the client never fetches or compares versions itself.
   */
  updateStatus: RuntimeUpdateInfo;
  isOnline: boolean;
  /**
   * Non-null when the daemon process was spawned by a managed launcher
   * (e.g. "desktop" for the Electron app). In that case the CLI binary
   * is shipped and upgraded by the launcher itself, so in-app self-update
   * is disabled — upgrading would be clobbered on the next launch anyway.
   */
  launchedBy?: string | null;
}

export function UpdateSection({
  runtimeId,
  currentVersion,
  updateStatus,
  isOnline,
  launchedBy,
}: UpdateSectionProps) {
  const { t } = useT("runtimes");
  const isManaged = launchedBy === "desktop";
  const [status, setStatus] = useState<RuntimeUpdateStatus | null>(null);
  const [error, setError] = useState("");
  const [output, setOutput] = useState("");
  const [updating, setUpdating] = useState(false);
  const [targetVersion, setTargetVersion] = useState<string | null>(null);
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const cleanup = useCallback(() => {
    if (pollRef.current) {
      clearInterval(pollRef.current);
      pollRef.current = null;
    }
  }, []);

  useEffect(() => cleanup, [cleanup]);

  const markCompleted = useCallback(
    (message: string) => {
      setStatus("completed");
      setOutput(message);
      setUpdating(false);
      setTargetVersion(null);
      cleanup();
      // Auto-clear status after a few seconds so the UI refreshes to show the
      // new version from the re-fetched runtime data.
      setTimeout(() => setStatus(null), 5000);
    },
    [cleanup],
  );

  useEffect(() => {
    if (!updating || !targetVersion) return;
    if (!updateStatus.available) {
      markCompleted(`Updated to ${targetVersion}`);
    }
  }, [markCompleted, targetVersion, updateStatus.available, updating]);

  const handleUpdate = async () => {
    if (!hasUpdate || !runtimeId) return;
    const target = updateTarget;
    cleanup();
    setUpdating(true);
    setTargetVersion(target);
    setStatus("pending");
    setError("");
    setOutput("");

    try {
      const update = await api.initiateUpdate(runtimeId, target);

      pollRef.current = setInterval(async () => {
        try {
          const result = await api.getUpdateResult(runtimeId, update.id);
          setStatus(result.status as RuntimeUpdateStatus);

          if (result.status === "completed") {
            markCompleted(
              result.output ?? `Updated to ${target}`,
            );
          } else if (
            result.status === "failed" ||
            result.status === "timeout"
          ) {
            setError(result.error ?? t(($) => $.update.unknown_error));
            setUpdating(false);
            setTargetVersion(null);
            cleanup();
          }
        } catch {
          // ignore poll errors
        }
      }, 2000);
    } catch {
      setStatus("failed");
      setError(t(($) => $.update.initiate_failed));
      setUpdating(false);
      setTargetVersion(null);
    }
  };

  const hasUpdate = updateStatus.available;
  // The daemon's fork update ignores the target and installs the server's own
  // build; the target is only what the request and the success text name.
  const updateTarget = updateStatus.serverVersion || updateStatus.serverCommit;
  const shortCommit = updateStatus.serverCommit.slice(0, 7);
  const availableLabel = [
    updateStatus.serverVersion,
    shortCommit && `(${shortCommit})`,
  ]
    .filter(Boolean)
    .join(" ");

  // Only a firm verdict from the server may say "Latest": an unknown commit
  // (source build) or a server with no build for this platform is not a claim
  // we can make either way, and an older server sends no verdict at all.
  const isLocalBuild = updateStatus.reason === "unknown_commit";
  const isLatest =
    !hasUpdate &&
    !updateStatus.reason &&
    !!updateStatus.serverCommit &&
    !!currentVersion;

  const config = status ? statusConfig[status] : null;
  const Icon = config?.icon;
  const isActive = status === "pending" || status === "running";

  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2 flex-wrap">
        <span className="text-caption text-muted-foreground">{t(($) => $.update.cli_version_label)}</span>
        <span className="text-caption font-mono">
          {currentVersion ?? t(($) => $.update.version_unknown)}
        </span>

        {isManaged ? (
          <span
            className="inline-flex items-center gap-1 text-caption text-muted-foreground"
            title={t(($) => $.update.managed_by_desktop_title)}
          >
            {t(($) => $.update.managed_by_desktop)}
          </span>
        ) : (
          <>
            {isLocalBuild && !status && (
              <span
                className="inline-flex items-center gap-1 text-caption text-muted-foreground"
                title={t(($) => $.update.local_build_title)}
              >
                {t(($) => $.update.local_build)}
              </span>
            )}

            {isLatest && !status && (
                <span className="inline-flex items-center gap-1 text-caption text-success">
                  <Check className="h-3 w-3" />
                  {t(($) => $.update.latest)}
                </span>
              )}

            {hasUpdate && !status && (
              <>
                <span className="text-caption text-muted-foreground">→</span>
                <span className="text-caption font-mono text-info">
                  {availableLabel}
                </span>
                <span className="text-caption text-muted-foreground">{t(($) => $.update.available)}</span>
              </>
            )}

            {hasUpdate && !runtimeId && (
              <span
                className="inline-flex items-center gap-1 text-caption text-muted-foreground"
                title={t(($) => $.update.read_only_title)}
              >
                <Lock className="h-3 w-3" />
                {t(($) => $.update.read_only)}
              </span>
            )}

            {hasUpdate && runtimeId && isOnline && !status && (
              <Button
                variant="outline"
                size="xs"
                onClick={handleUpdate}
                disabled={updating}
              >
                <ArrowUpCircle className="h-3 w-3" />
                {t(($) => $.update.action)}
              </Button>
            )}
          </>
        )}

        {config && Icon && status && (
          <span
            className={`inline-flex items-center gap-1 text-caption ${config.color}`}
          >
            <Icon className={`h-3 w-3 ${isActive ? "animate-spin" : ""}`} />
            {t(($) => $.update.status[status])}
          </span>
        )}
      </div>

      {status === "completed" && output && (
        <div className="rounded-lg border bg-success/5 px-3 py-2">
          <p className="text-caption text-success">{output}</p>
        </div>
      )}

      {(status === "failed" || status === "timeout") && error && (
        <div className="rounded-lg border border-destructive/20 bg-destructive/5 px-3 py-2">
          <p className="text-caption text-destructive">{error}</p>
          {status === "failed" && (
            <Button
              variant="ghost"
              size="xs"
              className="mt-1"
              onClick={handleUpdate}
            >
              {t(($) => $.update.retry)}
            </Button>
          )}
        </div>
      )}
    </div>
  );
}
