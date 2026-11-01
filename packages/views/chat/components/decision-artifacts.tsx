import { useId, useRef, useState, type KeyboardEvent } from "react";
import { useQuery } from "@tanstack/react-query";
import { Check, Download, FileText, Loader2 } from "@multica/ui/icons";
import { api, PreviewTooLargeError } from "@multica/core/api";
import { countDiffLines, useDecisionArtifacts } from "@multica/core/issues/decision-artifacts";
import type { DecisionArtifact, DecisionArtifactDiff } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { formatBytes } from "../../common/format-bytes";
import { useDownloadAttachment } from "../../editor/use-download-attachment";
import { useT } from "../../i18n";
import { RichContent } from "../../rich-content";

type TabKey = "changes" | "preview";

const isMarkdown = ({ attachment }: DecisionArtifact) =>
  !!attachment && (attachment.content_type.startsWith("text/markdown") || /\.(md|markdown)$/i.test(attachment.filename));

// New-side line count of a hunk: context and added lines, not removed ones.
const newSideLines = (lines: string[]) => lines.filter((line) => !line.startsWith("-")).length;

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

function DiffView({ artifact, diff }: { artifact: DecisionArtifact; diff: DecisionArtifactDiff }) {
  const { t } = useT("chat");
  const { added, removed } = countDiffLines(diff.hunks);
  const previous = artifact.supersedes_signed_by;
  return (
    <div className="grid gap-2">
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
        <div
          role="region"
          tabIndex={0}
          aria-label={t(($) => $.decision.artifacts.diff_region, { previous: diff.previous_filename })}
          className="max-h-72 overflow-auto rounded-lg border border-surface-border bg-surface font-mono text-caption leading-6 focus-visible:outline-2 focus-visible:outline-ring"
        >
          {diff.hunks.map((hunk, index) => {
            const previous = diff.hunks[index - 1];
            const hidden = previous ? hunk.new_start - (previous.new_start + newSideLines(previous.lines)) : 0;
            let number = hunk.new_start;
            return (
              <div key={`${hunk.old_start}:${hunk.new_start}`}>
                {hidden > 0 && (
                  <div className="border-y border-dashed border-surface-border px-2.5 py-0.5 text-center italic text-muted-foreground">
                    {t(($) => $.decision.artifacts.lines_hidden, { count: hidden })}
                  </div>
                )}
                {hunk.lines.map((line, lineIndex) => {
                  const kind = line[0];
                  const shown = kind === "-" ? "" : number++;
                  return (
                    <div
                      key={lineIndex}
                      className={cn(
                        "grid grid-cols-[2rem_1.1rem_1fr] whitespace-pre-wrap break-words",
                        kind === "+" && "bg-success/10",
                        kind === "-" && "bg-destructive/10",
                      )}
                    >
                      <i className="pr-1.5 text-right not-italic text-muted-foreground">{shown}</i>
                      <s className={cn("text-center font-bold no-underline", kind === "+" && "text-success", kind === "-" && "text-destructive")}>
                        {kind === "+" ? "+" : kind === "-" ? "−" : ""}
                      </s>
                      <code>{line.slice(1)}</code>
                    </div>
                  );
                })}
              </div>
            );
          })}
        </div>
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
    <div className="grid gap-1.5">
      <div
        role="region"
        tabIndex={0}
        aria-label={t(($) => $.decision.artifacts.preview_region, { name })}
        className="max-h-60 overflow-auto rounded-lg border border-surface-border bg-surface px-3 py-2.5 focus-visible:outline-2 focus-visible:outline-ring"
      >
        <RichContent content={preview.data.text} density="compact" phase="settled" />
      </div>
      <p className="text-caption text-muted-foreground">{t(($) => $.decision.artifacts.preview_footer, { name, count: lineCount })}</p>
    </div>
  );
}

// One tabbed viewer: closed by default; selecting a tab opens it, selecting the open tab closes it.
function ArtifactViewer({ artifact, onRetryChanges }: { artifact: DecisionArtifact; onRetryChanges: () => void }) {
  const { t } = useT("chat");
  const id = useId();
  const [open, setOpen] = useState<TabKey | null>(null);
  const tabs: TabKey[] = [
    ...(artifact.diff ? (["changes"] as const) : []),
    ...(isMarkdown(artifact) ? (["preview"] as const) : []),
  ];
  const [focused, setFocused] = useState<TabKey | undefined>(tabs[0]);
  const refs = useRef<Partial<Record<TabKey, HTMLButtonElement | null>>>({});
  if (tabs.length === 0) return null;

  const move = (key: TabKey) => {
    setFocused(key);
    refs.current[key]?.focus();
  };
  const onKeyDown = (event: KeyboardEvent, key: TabKey) => {
    const index = tabs.indexOf(key);
    const target =
      event.key === "ArrowRight" ? tabs[(index + 1) % tabs.length]
      : event.key === "ArrowLeft" ? tabs[(index - 1 + tabs.length) % tabs.length]
      : event.key === "Home" ? tabs[0]
      : event.key === "End" ? tabs[tabs.length - 1]
      : undefined;
    if (!target) return;
    event.preventDefault();
    move(target);
  };
  const diff = artifact.diff;
  const counts = diff ? countDiffLines(diff.hunks) : undefined;
  const tabId = (key: TabKey) => `${id}-tab-${key}`;
  const panelId = (key: TabKey) => `${id}-panel-${key}`;

  return (
    <div data-testid="decision-artifact-viewer" className="overflow-hidden rounded-lg border border-surface-border bg-surface">
      <div role="tablist" aria-label={t(($) => $.decision.artifacts.viewer_label)} className="flex gap-0.5 overflow-x-auto border-b border-surface-border px-1">
        {tabs.map((key) => (
          <button
            key={key}
            ref={(node) => { refs.current[key] = node; }}
            type="button"
            role="tab"
            id={tabId(key)}
            aria-selected={open === key}
            aria-controls={open === key ? panelId(key) : undefined}
            tabIndex={focused === key ? 0 : -1}
            onFocus={() => setFocused(key)}
            onKeyDown={(event) => onKeyDown(event, key)}
            onClick={() => setOpen(open === key ? null : key)}
            className={cn(
              "inline-flex min-h-11 items-center gap-1.5 whitespace-nowrap border-b-[3px] px-2.5 text-label font-semibold focus-visible:outline-2 focus-visible:outline-ring",
              open === key ? "border-primary text-foreground" : "border-transparent text-muted-foreground",
            )}
          >
            {key === "changes" ? t(($) => $.decision.artifacts.tab_changes, { previous: diff?.previous_filename }) : t(($) => $.decision.artifacts.tab_preview)}
            {key === "changes" && counts && (
              <span className="text-caption font-normal text-muted-foreground">+{counts.added} −{counts.removed}</span>
            )}
          </button>
        ))}
      </div>
      {open ? (
        <div role="tabpanel" id={panelId(open)} aria-labelledby={tabId(open)} className="grid gap-2 px-3 py-2.5">
          {open === "changes" && diff && (
            diff.problem
              ? <RetryBanner message={t(($) => $.decision.artifacts.changes_failed)} onRetry={onRetryChanges} />
              : <DiffView artifact={artifact} diff={diff} />
          )}
          {open === "preview" && <PreviewView artifact={artifact} />}
        </div>
      ) : (
        <p className="px-3 py-2 text-caption text-muted-foreground">{t(($) => $.decision.artifacts.viewer_hint)}</p>
      )}
    </div>
  );
}

function ArtifactFile({ artifact, onRetryChanges }: { artifact: DecisionArtifact; onRetryChanges: () => void }) {
  const { t } = useT("chat");
  const { attachment } = artifact;
  const download = useDownloadAttachment();
  const signed = artifact.signed_by;
  return (
    <div data-testid="decision-artifact-file" className="grid gap-2">
      <div className="flex flex-wrap items-start gap-2.5">
        <span className="grid size-9 shrink-0 place-items-center rounded-lg bg-primary/10 text-primary">
          <FileText className="size-[18px]" aria-hidden />
        </span>
        <div className="grid min-w-36 flex-1 justify-items-start gap-0.5">
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
            </>
          ) : (
            <span className="text-caption text-muted-foreground">{t(($) => $.decision.artifacts.file_missing)}</span>
          )}
        </div>
        {attachment && (
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
        )}
      </div>
      <ArtifactViewer artifact={artifact} onRetryChanges={onRetryChanges} />
    </div>
  );
}

/**
 * The first section of a decision card that approves files (design
 * `you-are-approving-card` direction B): one file card per artifact, then a
 * tabbed viewer for the changes since the previous revision and a rendered
 * Markdown preview. An ask without files renders nothing.
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
