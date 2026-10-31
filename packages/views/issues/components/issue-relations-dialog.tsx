"use client";

import { useId, useState, type RefObject } from "react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { useIssueStatuses } from "@multica/core/issue-statuses/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { issueRelationsOptions } from "@multica/core/issues/relations";
import { useRelationsFilterStore } from "@multica/core/issues/stores/relations-filter-store";
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
  const mentionsEnabled = useRelationsFilterStore((state) => !state.hidden.includes("mention"));
  const query = useQuery(issueRelationsOptions(workspaceId, issueId, depth));
  const mentions = useQuery({ ...issueRelationsOptions(workspaceId, issueId, depth, true), enabled: mentionsEnabled });
  const graph = mentionsEnabled && mentions.data ? mentions.data : query.data;
  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose(); }}>
      <DialogContent data-testid="relations-dialog" finalFocus={finalFocusRef} className="flex h-[min(48rem,90dvh)] flex-col gap-3 sm:max-w-6xl">
        <DialogHeader className="flex-row flex-wrap items-center justify-between gap-2 pr-8">
          <DialogTitle>{t(($) => $.relations.title)} <span aria-hidden="true" className="ml-2 font-mono text-caption text-muted-foreground">{graph?.nodes.find((node) => node.id === issueId)?.identifier}</span></DialogTitle>
          <DialogDescription className="sr-only">{t(($) => $.relations.direction)}</DialogDescription>
        <div className="flex items-center gap-2 text-caption">
          <label htmlFor={depthId}>{t(($) => $.relations.depth)}</label>
          <select id={depthId} className="rounded-md border border-input bg-background px-2 py-1" value={depth} onChange={(event) => setDepth(event.target.value === "2" ? 2 : 1)}>
            <option value="1">1</option><option value="2">2</option>
          </select>
        </div>
        </DialogHeader>
        {!graph && query.isPending ? <IssueRelationsSkeleton /> : !graph ? (
          <div role="alert"><p>{t(($) => $.relations.load_failed)}</p><Button variant="outline" onClick={() => void query.refetch()}>{t(($) => $.relations.retry)}</Button></div>
        ) : (
          <>
            {mentionsEnabled && mentions.isError && <div role="alert" className="text-caption"><p>{t(($) => $.relations.load_failed)}</p><Button variant="outline" size="sm" onClick={() => void mentions.refetch()}>{t(($) => $.relations.retry)}</Button></div>}
            {graph.edges.length === 0 && <p className="text-caption text-muted-foreground">{t(($) => $.relations.empty)}</p>}
            {graph.omitted_count > 0 && <p role="status" className="text-caption text-muted-foreground">{t(($) => $.relations.omitted, { count: graph.omitted_count })}</p>}
            <IssueRelationsGraph graph={graph} currentIssueId={issueId} catalog={catalog} mentionsPending={mentionsEnabled && mentions.isFetching} hrefForIssue={paths.issueDetail} onNavigate={(targetId) => { onClose(); navigation.push(paths.issueDetail(targetId)); }} />
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}

function IssueRelationsSkeleton() {
  const { t } = useT("issues");
  return <div role="status" aria-label={t(($) => $.relations.loading)} className="flex min-h-0 flex-1 flex-col gap-3 motion-safe:animate-pulse">
    <span className="sr-only">{t(($) => $.relations.loading)}</span>
    <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">{Array.from({ length: 4 }, (_, index) => <div key={index} className="h-24 rounded-xl bg-muted" />)}</div>
    <div className="flex min-h-48 flex-1 items-center justify-center gap-6 rounded-xl border border-surface-border p-4"><div className="h-24 w-1/4 rounded-xl bg-muted" /><div className="h-32 w-1/3 rounded-xl bg-muted" /><div className="flex w-1/3 flex-col gap-2">{Array.from({ length: 5 }, (_, index) => <div key={index} className="h-8 rounded-md bg-muted" />)}</div></div>
  </div>;
}
