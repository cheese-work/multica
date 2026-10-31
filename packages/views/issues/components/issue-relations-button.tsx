"use client";

import { useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { issueRelationsOptions } from "@multica/core/issues/relations";
import { GitFork } from "@multica/ui/icons";
import { Button } from "@multica/ui/components/ui/button";
import { useT } from "../../i18n";

import IssueRelationsDialog from "./issue-relations-dialog";

export function IssueRelationsButton({ issueId }: { issueId: string }) {
  const { t } = useT("issues");
  const [openedIssue, setOpenedIssue] = useState<string | null>(null);
  const workspaceId = useWorkspaceId();
  const queryClient = useQueryClient();
  const buttonRef = useRef<HTMLButtonElement>(null);
  useEffect(() => setOpenedIssue(null), [issueId]);
  const prefetch = () => { void queryClient.prefetchQuery(issueRelationsOptions(workspaceId, issueId, 1)); };
  return (
    <div className="px-2">
      <Button data-testid="relations-button" ref={buttonRef} variant="outline" size="sm" className="w-full justify-start" onMouseEnter={prefetch} onFocus={prefetch} onClick={() => setOpenedIssue(issueId)}>
        <GitFork />
        {t(($) => $.relations.title)}
      </Button>
      {openedIssue === issueId && (
        <IssueRelationsDialog issueId={issueId} onClose={() => setOpenedIssue(null)} finalFocusRef={buttonRef} />
      )}
    </div>
  );
}
