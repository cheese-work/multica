"use client";

import { useState } from "react";
import { parseUnifiedDiff } from "@multica/core/attachments/revision-diff";
import { useAttachmentRevisionDiff } from "@multica/core/attachments/revision-diff-query";
import { Button } from "@multica/ui/components/ui/button";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@multica/ui/components/ui/dialog";
import { HunksRegion } from "../../chat/components/diff-hunks";
import { useT } from "../../i18n";

/** The "Diff" action on a card whose file has an earlier revision. */
export function AttachmentRevisionDiffButton({
  attachmentId,
  filename,
  revision,
}: {
  attachmentId: string;
  filename: string;
  revision: number;
}) {
  const { t } = useT("issues");
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button variant="ghost" size="xs" onClick={() => setOpen(true)}>
        {t(($) => $.attachment_revision.diff)}
      </Button>
      {open && (
        <AttachmentRevisionDiffDialog
          attachmentId={attachmentId}
          filename={filename}
          revision={revision}
          onClose={() => setOpen(false)}
        />
      )}
    </>
  );
}

/** Changes from the revision before `revision` to `revision`, in the same red/green view as the artifact diff. */
function AttachmentRevisionDiffDialog({
  attachmentId,
  filename,
  revision,
  onClose,
}: {
  attachmentId: string;
  filename: string;
  revision: number;
  onClose: () => void;
}) {
  const { t } = useT("issues");
  const [full, setFull] = useState(false);
  const from = revision - 1;
  const diff = useAttachmentRevisionDiff(attachmentId, from, revision, full);
  const hunks = diff.data && !diff.data.binary ? parseUnifiedDiff(diff.data.diff) : [];

  return (
    <Dialog open onOpenChange={(next) => !next && onClose()}>
      <DialogContent className="flex max-h-[80vh] w-[min(52rem,calc(100vw-2rem))] max-w-none flex-col gap-3 sm:max-w-none">
        <DialogHeader>
          <DialogTitle>
            {t(($) => $.attachment_revision.title, { name: filename, from, to: revision })}
          </DialogTitle>
        </DialogHeader>
        {diff.isError ? (
          <p className="text-body text-muted-foreground" role="alert">
            {t(($) => $.attachment_revision.load_failed)}
          </p>
        ) : !diff.data ? (
          <p className="text-body text-muted-foreground" role="status">
            {t(($) => $.attachment_revision.loading)}
          </p>
        ) : diff.data.binary ? (
          <p className="text-body">{diff.data.message}</p>
        ) : hunks.length === 0 ? (
          <p className="text-body text-muted-foreground">{t(($) => $.attachment_revision.no_changes)}</p>
        ) : (
          <HunksRegion
            hunks={hunks}
            label={t(($) => $.attachment_revision.region, { from, to: revision })}
          />
        )}
        {diff.data && !diff.data.binary && (
          <div>
            <Button variant="outline" size="sm" onClick={() => setFull((v) => !v)}>
              {full
                ? t(($) => $.attachment_revision.show_changes)
                : t(($) => $.attachment_revision.show_full)}
            </Button>
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}
