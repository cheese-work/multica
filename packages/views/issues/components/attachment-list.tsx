"use client";

import { cn } from "@multica/ui/lib/utils";
import {
  orderStandaloneAttachments,
  selectStandaloneAttachments,
  standaloneAttachmentGroup,
} from "@multica/core/attachments/image-sequence";
import type { Attachment } from "@multica/core/types";
import { Attachment as AttachmentRenderer } from "../../editor/attachment";
import { AttachmentDownloadProvider } from "../../editor/attachment-download-context";
import { useAttachmentVersions } from "./deliverables/attachment-versions";
import { VersionBadge } from "./deliverables/version-badge";
import { AttachmentRevisionDiffButton } from "./attachment-revision-diff";
import { useT } from "../../i18n";

// Standalone attachment list — renders attachments not already in the markdown

export function AttachmentList({
  attachments,
  content,
  className,
  onRemove,
}: {
  attachments?: Attachment[];
  content?: string;
  className?: string;
  onRemove?: (attachmentId: string) => void;
}) {
  const versions = useAttachmentVersions();
  const { t } = useT("issues");
  if (!attachments?.length) return null;
  // Skip attachments whose URL (stable or legacy) is already referenced in the
  // markdown content, and duplicates of the same file that are referenced.
  // Shared with the image-sequence builder (MUL-5752) so the cards rendered
  // here and the images the viewer pages through can't disagree — as is the
  // grouping below, so paging follows the screen (MUL-7649).
  // A document uploaded again shows once, as its latest revision.
  const superseded = new Set(attachments.map((a) => a.revision_of).filter(Boolean));
  const standalone = orderStandaloneAttachments(
    selectStandaloneAttachments(
      content,
      attachments.filter((a) => !superseded.has(a.id)),
    ),
  );
  if (!standalone.length) return null;

  const images = standalone.filter((a) => standaloneAttachmentGroup(a) === "image");
  const files = standalone.filter((a) => standaloneAttachmentGroup(a) === "file");
  const render = (a: Attachment) => {
    const version = versions?.get(a.id);
    const revision = a.revision ?? 1;
    const chained = revision >= 2;
    return (
      <AttachmentRenderer
        key={a.id}
        attachment={{ kind: "record", attachment: a }}
        layout="card"
        badge={
          chained ? (
            <span
              className="shrink-0 rounded-xs bg-muted px-1 text-micro font-medium tabular-nums text-muted-foreground"
              aria-hidden
            >
              {t(($) => $.attachment_revision.badge, { revision, earlier: revision - 1 })}
            </span>
          ) : version ? (
            <VersionBadge version={version} />
          ) : undefined
        }
        actions={
          chained ? (
            <AttachmentRevisionDiffButton
              attachmentId={a.id}
              filename={a.filename}
              revision={revision}
            />
          ) : undefined
        }
        editable={!!onRemove}
        onDelete={onRemove ? () => onRemove(a.id) : undefined}
      />
    );
  };

  // Every image shows at its full size, one under another, so it reads
  // without opening (MUL-7736). Every other file — HTML too: an uploaded file
  // is a deliverable to open, not part of the text — is a card in a grid, not
  // a full-width row each.
  return (
    <AttachmentDownloadProvider attachments={attachments}>
      <div className={cn("flex flex-col gap-2", className)}>
        {images.length > 0 && (
          <div className="flex flex-col items-start gap-2">{images.map(render)}</div>
        )}
        {files.length > 0 && (
          <div className="grid grid-cols-[repeat(auto-fill,minmax(min(15rem,100%),1fr))] gap-2">
            {files.map(render)}
          </div>
        )}
      </div>
    </AttachmentDownloadProvider>
  );
}
