"use client";

import { lazy, Suspense, useRef, useState } from "react";
import { GitFork } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { useT } from "../../i18n";

const IssueRelationsDialog = lazy(() => import("./issue-relations-dialog"));

export function IssueRelationsButton({ issueId }: { issueId: string }) {
  const { t } = useT("issues");
  const [openedIssue, setOpenedIssue] = useState<string | null>(null);
  const buttonRef = useRef<HTMLButtonElement>(null);
  return (
    <div className="px-2">
      <Button ref={buttonRef} variant="outline" size="sm" className="w-full justify-start" onClick={() => setOpenedIssue(issueId)}>
        <GitFork />
        {t(($) => $.relations.title)}
      </Button>
      {openedIssue === issueId && (
        <Suspense fallback={<p role="status" className="text-caption text-muted-foreground">{t(($) => $.relations.loading)}</p>}>
          <IssueRelationsDialog issueId={issueId} onClose={() => setOpenedIssue(null)} finalFocusRef={buttonRef} />
        </Suspense>
      )}
    </div>
  );
}
