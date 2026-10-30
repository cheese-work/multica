"use client";

import { useId, useState, type RefObject } from "react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { useIssueStatuses } from "@multica/core/issue-statuses/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { issueRelationsOptions } from "@multica/core/issues/relations";
import { Button } from "@multica/ui/components/ui/button";
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription } from "@multica/ui/components/ui/dialog";
import { useNavigation } from "../../navigation";
import { useT } from "../../i18n";
import { IssueRelationsGraph } from "./issue-relations-graph";

export default function IssueRelationsDialog({ issueId, onClose, finalFocusRef }: {
  issueId: string;
  onClose: () => void;
  finalFocusRef: RefObject<HTMLButtonElement | null>;
}) {
  const { t } = useT("issues");
  const workspaceId = useWorkspaceId();
  const catalog = useIssueStatuses(workspaceId);
  const paths = useWorkspacePaths();
  const navigation = useNavigation();
  const [depth, setDepth] = useState<1 | 2>(1);
  const depthId = useId();
  const query = useQuery(issueRelationsOptions(workspaceId, issueId, depth));
  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose(); }}>
      <DialogContent finalFocus={finalFocusRef} className="flex h-[min(48rem,90dvh)] flex-col sm:max-w-5xl">
        <DialogHeader>
          <DialogTitle>{t(($) => $.relations.title)}</DialogTitle>
          <DialogDescription>{t(($) => $.relations.direction)}</DialogDescription>
        </DialogHeader>
        <div className="flex items-center gap-2 text-caption">
          <label htmlFor={depthId}>{t(($) => $.relations.depth)}</label>
          <select id={depthId} className="rounded-md border border-input bg-background px-2 py-1" value={depth} onChange={(event) => setDepth(event.target.value === "2" ? 2 : 1)}>
            <option value="1">1</option><option value="2">2</option>
          </select>
        </div>
        {query.isPending ? <p role="status">{t(($) => $.relations.loading)}</p> : query.isError ? (
          <div role="alert"><p>{t(($) => $.relations.load_failed)}</p><Button variant="outline" onClick={() => void query.refetch()}>{t(($) => $.relations.retry)}</Button></div>
        ) : (
          <>
            {query.data.edges.length === 0 && <p className="text-caption text-muted-foreground">{t(($) => $.relations.empty)}</p>}
            {query.data.omitted_count > 0 && <p role="status" className="text-caption text-muted-foreground">{t(($) => $.relations.omitted, { count: query.data.omitted_count })}</p>}
            <IssueRelationsGraph graph={query.data} currentIssueId={issueId} catalog={catalog} onNavigate={(targetId) => { onClose(); navigation.push(paths.issueDetail(targetId)); }} />
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}
