import { useCallback, useEffect, useId, useRef, useState, type KeyboardEvent } from "react";
import { useQuery } from "@tanstack/react-query";
import { Check, Download, FileText, Loader2 } from "@multica/ui/icons";
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from "@multica/ui/components/ui/dialog";
import { api, PreviewTooLargeError } from "@multica/core/api";
import { countDiffLines, useDecisionArtifacts } from "@multica/core/issues/decision-artifacts";
import type { DecisionArtifact, DecisionArtifactDiff } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { formatBytes } from "../../common/format-bytes";
import { useDownloadAttachment } from "../../editor/use-download-attachment";
import { useT } from "../../i18n";
import { RichContent } from "../../rich-content";
import { HunksRegion } from "./diff-hunks";

type TabKey = "changes" | "preview";

const isMarkdown = ({ attachment }: DecisionArtifact) =>
  !!attachment && (attachment.content_type.startsWith("text/markdown") || /\.(md|markdown)$/i.test(attachment.filename));
const isText = ({ attachment }: DecisionArtifact) =>
  !!attachment && (attachment.content_type.startsWith("text/") || /\.(txt|log)$/i.test(attachment.filename));

// Browser Back closes the open viewer instead of leaving the page: one history entry while open, kept
// on top of the current state so the router still recognises it. A normal close pops that entry.
function useBackToClose(open: boolean, close: () => void) {
  useEffect(() => {
    if (!open) return;
    window.history.pushState({ ...window.history.state, artifactViewer: true }, "");
    let popped = false;
    const onPop = () => { popped = true; close(); };
    window.addEventListener("popstate", onPop);
    return () => {
      window.removeEventListener("popstate", onPop);
      if (!popped && window.history.state?.artifactViewer) window.history.back();
    };
  }, [open, close]);
}

function RetryBanner({ message, onRetry }: { message: string; onRetry: () => void }) {
  const { t } = useT("chat");
  return (
    <div role="alert" className="flex flex-wrap items-center justify-between gap-2 rounded-lg bg-destructive/10 px-2.5 py-2 text-caption font-medium text-destructive">
      <span>{message}</span>
      <button type="button" onClick={onRetry} className="min-h-9 rounded-lg border border-surface-border bg-surface px-3 text-label font-medium text-foreground focus-visible:outline-2 focus-visible:outline-ring">
        {t(($) => $.decision.artifacts.retry)}
      </button>
    </div>
  );
}

function SignatureTag({ signed, testId, children }: { signed: boolean; testId?: string; children: string }) {
  return (
    <span data-testid={testId} className={cn("inline-flex items-center gap-1 rounded-full px-2 text-micro font-semibold", signed ? "bg-success/10 text-success" : "bg-warning/10 text-warning")}>
      {signed && <Check className="size-3" aria-hidden />}
      {children}
    </span>
  );
}

function DiffView({ diff, artifact }: { artifact: DecisionArtifact; diff: DecisionArtifactDiff }) {
  const { t } = useT("chat");
  const { added, removed } = countDiffLines(diff.hunks);
  const previous = artifact.supersedes_signed_by;
  return (
    <div className="flex min-h-0 flex-1 flex-col gap-2">
      <p className="flex flex-wrap items-center gap-x-2 gap-y-1.5 text-caption">
        <SignatureTag signed={!!previous}>
          {previous
            ? t(($) => $.decision.artifacts.previous_signed_by, { previous: diff.previous_filename, reviewer: previous.reviewer })
            : t(($) => $.decision.artifacts.previous_not_signed, { previous: diff.previous_filename })}
        </SignatureTag>
        <span>{t(($) => $.decision.artifacts.changes_summary, { added, removed })}</span>
      </p>
      {diff.hunks.length === 0 ? (
        <p className="text-caption text-muted-foreground">{t(($) => $.decision.artifacts.no_changes)}</p>
      ) : (
        <HunksRegion hunks={diff.hunks} label={t(($) => $.decision.artifacts.diff_region, { previous: diff.previous_filename })} />
      )}
    </div>
  );
}

function PreviewView({ artifact }: { artifact: DecisionArtifact }) {
  const { t } = useT("chat");
  const attachmentId = artifact.attachment!.id;
  const name = artifact.attachment!.filename;
  const preview = useQuery({
    queryKey: ["decision-artifact-preview", attachmentId],
    queryFn: () => api.getAttachmentTextContent(attachmentId),
    staleTime: 60_000,
  });
  if (preview.isPending) {
    return (
      <div role="status" aria-label={t(($) => $.decision.artifacts.loading_preview)} className="flex items-center gap-2 text-caption text-muted-foreground">
        <Loader2 className="size-4 animate-spin" aria-hidden />
        {t(($) => $.decision.artifacts.loading_preview)}
      </div>
    );
  }
  if (preview.isError) {
    const tooLarge = preview.error instanceof PreviewTooLargeError;
    return (
      <RetryBanner
        message={tooLarge ? t(($) => $.decision.artifacts.preview_too_large) : t(($) => $.decision.artifacts.preview_failed)}
        onRetry={() => void preview.refetch()}
      />
    );
  }
  const lineCount = preview.data.text.replace(/\n$/, "").split("\n").length;
  return (
    <div className="flex min-h-0 flex-1 flex-col gap-1.5">
      <div
        role="region"
        tabIndex={0}
        aria-label={t(($) => $.decision.artifacts.preview_region, { name })}
        className="min-h-0 flex-1 overflow-auto rounded-lg border border-surface-border bg-surface px-3 py-2.5 focus-visible:outline-2 focus-visible:outline-ring"
      >
        {isMarkdown(artifact)
          ? <RichContent content={preview.data.text} density="compact" phase="settled" />
          : <pre className="whitespace-pre-wrap break-words font-mono text-caption">{preview.data.text}</pre>}
      </div>
      <p className="text-caption text-muted-foreground">{t(($) => $.decision.artifacts.preview_footer, { name, count: lineCount })}</p>
    </div>
  );
}

const viewerTabs = (artifact: DecisionArtifact): TabKey[] => [
  ...(artifact.diff ? (["changes"] as const) : []),
  ...(isMarkdown(artifact) || isText(artifact) ? (["preview"] as const) : []),
];

// The fullscreen viewer (design `you-are-approving-card` direction B, CHE-2810). Phone: header on top,
// one open view, a bottom bar with the view tabs and Download. From `sm`: a left rail with the file
// facts, the view tabs and Download beside the open view. One tab is always open.
function ArtifactDialogContent({ artifact, attachment, onRetryChanges }: { artifact: DecisionArtifact; attachment: NonNullable<DecisionArtifact["attachment"]>; onRetryChanges: () => void }) {
  const { t } = useT("chat");
  const id = useId();
  const tabs = viewerTabs(artifact);
  const [open, setOpen] = useState<TabKey>(tabs[0]!);
  const [focused, setFocused] = useState<TabKey>(tabs[0]!);
  const refs = useRef<Partial<Record<TabKey, HTMLButtonElement | null>>>({});

  const onKeyDown = (event: KeyboardEvent, key: TabKey) => {
    const index = tabs.indexOf(key);
    const target =
      event.key === "ArrowRight" || event.key === "ArrowDown" ? tabs[(index + 1) % tabs.length]
      : event.key === "ArrowLeft" || event.key === "ArrowUp" ? tabs[(index - 1 + tabs.length) % tabs.length]
      : event.key === "Home" ? tabs[0]
      : event.key === "End" ? tabs[tabs.length - 1]
      : undefined;
    if (!target) return;
    event.preventDefault();
    setFocused(target);
    refs.current[target]?.focus();
  };
  const diff = artifact.diff;
  const counts = diff ? countDiffLines(diff.hunks) : undefined;
  const tabId = (key: TabKey) => `${id}-tab-${key}`;

  return (
    <DialogContent data-testid="decision-artifact-dialog" className="h-dvh max-h-none w-full max-w-none grid-rows-[auto_minmax(0,1fr)_auto] gap-0 rounded-none p-0 sm:h-[85dvh] sm:max-w-4xl sm:grid-cols-[17rem_minmax(0,1fr)] sm:grid-rows-[auto_minmax(0,1fr)] sm:rounded-xl">
      <DialogHeader className="p-4 pr-12 sm:col-start-1 sm:row-start-1 sm:border-r sm:border-surface-border sm:pr-4">
        <DialogTitle className="sr-only">{artifact.label}</DialogTitle>
        <div className="flex items-start gap-2.5">
          <ArtifactSummary artifact={artifact} />
        </div>
      </DialogHeader>
      <div
        role="tabpanel"
        id={`${id}-panel`}
        aria-labelledby={tabId(open)}
        data-testid="decision-artifact-viewer"
        className="flex min-h-0 flex-col border-y border-surface-border p-4 sm:col-start-2 sm:row-span-2 sm:row-start-1 sm:border-y-0"
      >
        {open === "changes" && diff && (
          diff.problem
            ? <RetryBanner message={t(($) => $.decision.artifacts.changes_failed)} onRetry={onRetryChanges} />
            : <DiffView artifact={artifact} diff={diff} />
        )}
        {open === "preview" && <PreviewView artifact={artifact} />}
      </div>
      <div className="flex items-center gap-1 p-2 sm:col-start-1 sm:row-start-2 sm:flex-col sm:items-stretch sm:self-start sm:border-r sm:border-surface-border sm:p-4">
        <div role="tablist" aria-label={t(($) => $.decision.artifacts.viewer_label)} className="flex min-w-0 flex-1 gap-0.5 overflow-x-auto sm:flex-none sm:flex-col">
          {tabs.map((key) => (
            <button
              key={key}
              ref={(node) => { refs.current[key] = node; }}
              type="button"
              role="tab"
              id={tabId(key)}
              aria-selected={open === key}
              aria-controls={`${id}-panel`}
              tabIndex={focused === key ? 0 : -1}
              onFocus={() => setFocused(key)}
              onKeyDown={(event) => onKeyDown(event, key)}
              onClick={() => setOpen(key)}
              className={cn(
                "inline-flex min-h-14 flex-1 items-center justify-center gap-1.5 whitespace-nowrap rounded-lg px-3 text-label font-semibold focus-visible:outline-2 focus-visible:outline-ring sm:min-h-11 sm:flex-none sm:justify-start",
                open === key ? "bg-primary/10 text-foreground" : "text-muted-foreground hover:bg-muted",
              )}
            >
              {key === "changes" ? t(($) => $.decision.artifacts.tab_changes) : t(($) => $.decision.artifacts.tab_preview)}
              {key === "changes" && counts && (
                <span className="text-caption font-normal text-muted-foreground">+{counts.added} −{counts.removed}</span>
              )}
            </button>
          ))}
        </div>
        <DownloadButton attachment={attachment} />
      </div>
    </DialogContent>
  );
}

function DownloadButton({ attachment }: { attachment: NonNullable<DecisionArtifact["attachment"]> }) {
  const { t } = useT("chat");
  const download = useDownloadAttachment();
  return (
    <button
      type="button"
      data-testid="decision-artifact-download"
      onClick={() => void download(attachment.id)}
      aria-label={t(($) => $.decision.artifacts.download_label, { name: attachment.filename })}
      className="inline-flex min-h-11 items-center gap-1.5 rounded-lg border border-surface-border bg-surface px-3.5 text-label font-semibold hover:bg-muted focus-visible:outline-2 focus-visible:outline-ring"
    >
      <Download className="size-4" aria-hidden />
      {t(($) => $.decision.artifacts.download)}
    </button>
  );
}

// Spans only: the card wraps this in a button when the file can be opened.
function ArtifactSummary({ artifact, showViews = false }: { artifact: DecisionArtifact; showViews?: boolean }) {
  const { t } = useT("chat");
  const { attachment } = artifact;
  const signed = artifact.signed_by;
  const counts = artifact.diff && countDiffLines(artifact.diff.hunks);
  const views = viewerTabs(artifact).map((key) =>
    key === "changes" && counts ? `${t(($) => $.decision.artifacts.tab_changes)} +${counts.added} −${counts.removed}` : t(($) => $.decision.artifacts.tab_preview),
  );
  return (
    <>
      <span className="grid size-9 shrink-0 place-items-center rounded-lg bg-primary/10 text-primary">
        <FileText className="size-[18px]" aria-hidden />
      </span>
      <span className="grid min-w-36 flex-1 justify-items-start gap-0.5 text-left">
        <b className="text-body">{artifact.label}</b>
        {attachment ? (
          <>
            <span className="text-caption text-muted-foreground">{attachment.filename} · {formatBytes(attachment.size_bytes)}</span>
            <span className="text-caption text-muted-foreground">
              {t(($) => $.decision.artifacts.sha)} <code className="font-mono">{artifact.sha256.slice(0, 7)}</code> ·{" "}
              <em className="inline-flex items-center gap-0.5 font-semibold not-italic text-success">
                <Check className="size-3" aria-hidden />
                {t(($) => $.decision.artifacts.hash_verified)}
              </em>
            </span>
            <SignatureTag signed={!!signed} testId="decision-artifact-signature">
              {signed
                ? t(($) => $.decision.artifacts.signed_by, { label: artifact.label, reviewer: signed.reviewer })
                : t(($) => $.decision.artifacts.not_signed, { label: artifact.label })}
            </SignatureTag>
            {showViews && <span className="text-caption font-semibold text-primary">{views.join(" · ")}</span>}
          </>
        ) : (
          <span className="text-caption text-muted-foreground">{t(($) => $.decision.artifacts.file_missing)}</span>
        )}
      </span>
    </>
  );
}

function ArtifactFile({ artifact, onRetryChanges }: { artifact: DecisionArtifact; onRetryChanges: () => void }) {
  const { t } = useT("chat");
  const { attachment } = artifact;
  const openable = !!attachment && viewerTabs(artifact).length > 0;
  const [open, setOpen] = useState(false);
  const close = useCallback(() => setOpen(false), []);
  useBackToClose(open, close);
  return (
    <div data-testid="decision-artifact-file" className="flex flex-wrap items-start gap-2.5">
      {openable ? (
        <Dialog open={open} onOpenChange={setOpen}>
          <DialogTrigger
            data-testid="decision-artifact-open"
            aria-label={t(($) => $.decision.artifacts.open_label, { name: attachment.filename })}
            className="-m-1 flex min-w-0 flex-1 basis-48 items-start gap-2.5 rounded-lg p-1 text-left hover:bg-muted focus-visible:outline-2 focus-visible:outline-ring"
          >
            <ArtifactSummary artifact={artifact} showViews />
          </DialogTrigger>
          <ArtifactDialogContent artifact={artifact} attachment={attachment} onRetryChanges={onRetryChanges} />
        </Dialog>
      ) : (
        <div className="flex min-w-0 flex-1 basis-48 items-start gap-2.5">
          <ArtifactSummary artifact={artifact} />
        </div>
      )}
      {attachment && <DownloadButton attachment={attachment} />}
    </div>
  );
}

/**
 * The first section of a decision card that approves files (design
 * `you-are-approving-card` direction B): one file card per artifact. Tapping
 * the file opens a fullscreen viewer with the changes since the previous
 * revision and a rendered Markdown preview. An ask without files renders nothing.
 */
export function DecisionArtifacts({ requestId, answered }: { requestId: string; answered: boolean }) {
  const { t } = useT("chat");
  const artifacts = useDecisionArtifacts(requestId);
  if (artifacts.isError) {
    return <RetryBanner message={t(($) => $.decision.artifacts.list_failed)} onRetry={() => void artifacts.refetch()} />;
  }
  if (!artifacts.data?.length) return null;
  const heading = answered ? t(($) => $.decision.artifacts.approved) : t(($) => $.decision.artifacts.approving);
  return (
    <section aria-label={heading} data-testid="decision-request-artifacts" className="grid gap-3 rounded-lg border border-surface-border bg-muted/40 p-2.5">
      <h3 className="text-micro font-bold uppercase tracking-wide text-muted-foreground">{heading}</h3>
      {artifacts.data.map((artifact) => (
        <ArtifactFile key={artifact.attachment_id} artifact={artifact} onRetryChanges={() => void artifacts.refetch()} />
      ))}
    </section>
  );
}
