"use client";

import { useMemo, useRef, useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { ArrowDownToLine, ArrowDown, ArrowUp, Ellipsis, ExternalLink, X } from "lucide-react";
import { api } from "@multica/core/api";
import { defaultPlanBase, diffRevisionLines, earlierPlanRevisions, planRevisionNumber, revisionSegments, type RevisionLine } from "@multica/core/attachments";
import { paths, useWorkspaceSlug } from "@multica/core/paths";
import { useActorName } from "@multica/core/workspace/hooks";
import type { Attachment } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { Switch } from "@multica/ui/components/ui/switch";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@multica/ui/components/ui/select";
import { DropdownMenu, DropdownMenuContent, DropdownMenuGroup, DropdownMenuItem, DropdownMenuLabel, DropdownMenuRadioGroup, DropdownMenuRadioItem, DropdownMenuTrigger } from "@multica/ui/components/ui/dropdown-menu";
import { cn } from "@multica/ui/lib/utils";
import { AppLink } from "../navigation";
import { useT } from "../i18n";
import { useAttachmentHtmlText } from "./hooks/use-attachment-html-text";
import { ReadonlyContent } from "./readonly-content";

export function PlanRevisionViewer({ attachment, onDownload, onClose, onOpenInNewTab, titleAccessory, info, locate, onOpenOverview }: {
  attachment: Attachment;
  onDownload: () => void;
  onClose?: () => void;
  onOpenInNewTab?: () => void;
  titleAccessory?: ReactNode;
  info?: { content: ReactNode; open: boolean; onToggle: () => void };
  locate?: { label: string; onSelect: () => void };
  onOpenOverview?: () => void;
}) {
  const { t } = useT("editor");
  const slug = useWorkspaceSlug();
  const { getActorName } = useActorName();
  const [requested, setRequested] = useState(false);
  const [baseChoice, setBaseChoice] = useState<string>();
  const [mode, setMode] = useState("unify");
  const [change, setChange] = useState(0);
  const [expanded, setExpanded] = useState<Set<number>>(new Set());
  const anchors = useRef<(HTMLElement | null)[]>([]);
  const contentPane = useRef<HTMLElement>(null);
  const diffHeader = useRef<HTMLDivElement>(null);
  const current = useAttachmentHtmlText(attachment.id);
  const attachments = useQuery({
    queryKey: ["plan-revisions", attachment.workspace_id, attachment.issue_id],
    queryFn: () => api.listAttachments(attachment.issue_id!),
    enabled: !!attachment.issue_id,
    retry: false,
  });
  const issue = useQuery({
    queryKey: ["plan-revision-issue", attachment.workspace_id, attachment.issue_id],
    queryFn: () => api.getIssue(attachment.issue_id!),
    enabled: !!attachment.issue_id,
    retry: false,
  });
  const revisions = useMemo(() => earlierPlanRevisions(attachment, attachments.data ?? []), [attachment, attachments.data]);
  const baseId = baseChoice ?? defaultPlanBase(revisions);
  const selected = revisions.find((revision) => revision.attachment.id === baseId);
  const base = useAttachmentHtmlText(requested && selected ? selected.attachment.id : null);
  const comparison = useMemo(() => {
    if (!requested || !selected || current.data === undefined || base.data === undefined || current.isError || base.isError) return null;
    try {
      const lines = diffRevisionLines(base.data.text, current.data.text);
      return { lines, segments: revisionSegments(lines), tooLarge: false };
    } catch {
      return { lines: [], segments: [], tooLarge: true };
    }
  }, [requested, selected, current.data, current.isError, base.data, base.isError]);
  const showingDiff = !!comparison && !comparison.tooLarge;
  const changes = comparison?.segments.filter((segment) => segment.kind === "change").length ?? 0;
  const additions = comparison?.lines.filter((line) => line.kind === "add").length ?? 0;
  const removals = comparison?.lines.filter((line) => line.kind === "remove").length ?? 0;
  const label = (candidate: Attachment) => `${candidate.filename} · ${new Intl.DateTimeFormat("en-GB", { timeZone: "Asia/Bangkok", day: "2-digit", month: "short", year: "numeric", hour: "2-digit", minute: "2-digit", hour12: false }).format(new Date(candidate.created_at))} ICT · ${getActorName(candidate.uploader_type, candidate.uploader_id)} · ${candidate.id}`;
  const chooseBase = (value: string) => {
    setBaseChoice(value);
    setChange(0);
    setExpanded(new Set());
  };
  const navigate = (direction: number) => {
    const next = (change + direction + changes) % changes;
    setChange(next);
    const section = anchors.current[next];
    const pane = contentPane.current;
    if (section && pane) {
      pane.scrollTop += section.getBoundingClientRect().top - pane.getBoundingClientRect().top - (diffHeader.current?.offsetHeight ?? 0);
      section.focus({ preventScroll: true });
    }
  };
  const modeControls = <>
    <Button variant="ghost" size="sm" className="aria-pressed:bg-muted" aria-pressed={mode === "split"} onClick={() => setMode("split")}>{t(($) => $.plan_revision.split)}</Button>
    <Button variant="ghost" size="sm" className="aria-pressed:bg-muted" aria-pressed={mode === "unify"} onClick={() => setMode("unify")}>{t(($) => $.plan_revision.unify)}</Button>
  </>;
  const basePicker = <div className="flex min-w-0 items-center gap-1 text-caption" data-testid="revision-base">
    <span className="shrink-0">{t(($) => $.plan_revision.compare)}</span>
    <Select items={revisions.map((revision) => ({ value: revision.attachment.id, label: t(($) => $.plan_revision.revision, { number: revision.revision }) }))} value={selected?.attachment.id ?? null} onValueChange={(value) => { if (value) chooseBase(value); }} disabled={!revisions.length}>
      <SelectTrigger size="sm" aria-label={t(($) => $.plan_revision.compare)} className="min-w-0 max-w-32 text-caption"><SelectValue placeholder={t(($) => $.plan_revision.choose_base)} /></SelectTrigger>
      <SelectContent align="end" alignItemWithTrigger={false} className="w-96 max-w-[calc(100vw-16px)]">
        {revisions.map(({ attachment: candidate }) => <SelectItem key={candidate.id} value={candidate.id} className="whitespace-normal break-words text-caption">{label(candidate)}</SelectItem>)}
      </SelectContent>
    </Select>
  </div>;
  return <div className="flex h-full min-h-0 w-full flex-col bg-background text-foreground">
    <header data-testid="revision-toolbar" className="flex h-12 shrink-0 items-center gap-2 border-b px-3 max-[699px]:gap-1 max-[699px]:px-2" style={{ WebkitAppRegion: "no-drag" } as React.CSSProperties}>
      <div className="flex min-w-0 flex-1 items-center gap-2">
        <span className="truncate text-label font-medium" title={attachment.filename}>{attachment.filename}</span>
        {titleAccessory}
        {slug && attachment.issue_id && <AppLink className="shrink-0 text-caption text-muted-foreground hover:underline max-[999px]:hidden" href={paths.workspace(slug).issueDetail(attachment.issue_id)}>{issue.data?.identifier ?? t(($) => $.plan_revision.issue)}</AppLink>}
        <span className="shrink-0 text-caption text-muted-foreground max-[1199px]:hidden">{t(($) => $.plan_revision.revision, { number: planRevisionNumber(attachment.filename) })}</span>
        <span className="shrink-0 rounded-sm border px-1.5 text-caption text-muted-foreground max-[1199px]:hidden">{t(($) => $.plan_revision.readonly)}</span>
      </div>
      <div className="flex shrink-0 items-center gap-1.5 text-caption">
        {t(($) => $.plan_revision.diff)}
        <Switch size="sm" aria-label={t(($) => $.plan_revision.diff)} checked={showingDiff} disabled={!selected || current.data === undefined || current.isError} onCheckedChange={() => {
          setRequested(!showingDiff && !(requested && base.isFetching));
          setChange(0);
          if (base.isError) void base.refetch();
        }} />
      </div>
      {showingDiff && <span className="shrink-0 text-caption tabular-nums max-[699px]:hidden"><span className="text-success">+{additions}</span> | <span className="text-destructive">−{removals}</span></span>}
      <div className="flex shrink-0 items-center gap-0.5" aria-live="polite">
        <span className="whitespace-nowrap text-caption tabular-nums">{t(($) => $.plan_revision.change, { current: changes ? change + 1 : 0, total: changes })}</span>
        <Button variant="ghost" size="icon-sm" aria-label={t(($) => $.plan_revision.previous)} disabled={!showingDiff || !changes} onClick={() => navigate(-1)}><ArrowUp className="size-3.5" /></Button>
        <Button variant="ghost" size="icon-sm" aria-label={t(($) => $.plan_revision.next)} disabled={!showingDiff || !changes} onClick={() => navigate(1)}><ArrowDown className="size-3.5" /></Button>
      </div>
      <div className="flex min-w-0 items-center gap-1 max-[699px]:hidden">{modeControls}{basePicker}
        <Button variant="ghost" size="icon-sm" data-testid="revision-download" aria-label={t(($) => $.plan_revision.download)} onClick={onDownload}><ArrowDownToLine className="size-4" /></Button>
      </div>
      <DropdownMenu>
        <DropdownMenuTrigger render={<Button variant="ghost" size="icon-sm" aria-label={t(($) => $.plan_revision.more)} />}><Ellipsis className="size-4" /></DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="max-w-[calc(100vw-16px)]">
          <DropdownMenuGroupPhone mode={mode} setMode={setMode} revisions={revisions.map(({ attachment: candidate }) => ({ id: candidate.id, label: label(candidate) }))} baseId={selected?.attachment.id ?? ""} chooseBase={chooseBase} onDownload={onDownload} />
          {onOpenInNewTab && <DropdownMenuItem onClick={onOpenInNewTab}><ExternalLink className="size-4" />{t(($) => $.attachment.open_in_new_tab)}</DropdownMenuItem>}
          {locate && <DropdownMenuItem onClick={locate.onSelect}>{locate.label}</DropdownMenuItem>}
          {info && <DropdownMenuItem onClick={info.onToggle}>{t(($) => $.attachment.info)}</DropdownMenuItem>}
          {onOpenOverview && <DropdownMenuItem onClick={onOpenOverview}>{t(($) => $.attachment.overview)}</DropdownMenuItem>}
          <DropdownMenuGroup><DropdownMenuLabel>{t(($) => $.plan_revision.readonly)}</DropdownMenuLabel></DropdownMenuGroup>
        </DropdownMenuContent>
      </DropdownMenu>
      {onClose && <Button variant="ghost" size="icon-sm" data-testid="revision-close" aria-label={t(($) => $.plan_revision.close)} onClick={onClose}><X className="size-4" /></Button>}
    </header>
    <div className="flex min-h-0 flex-1">
      <main ref={contentPane} data-testid="revision-content" className="min-w-0 flex-1 overflow-auto">
        {attachments.isPending && <p role="status" className="px-4 py-2 text-caption">{t(($) => $.plan_revision.loading_revisions)}</p>}
        {attachments.isError && <p role="alert" className="px-4 py-2 text-caption">{t(($) => $.plan_revision.revisions_error)} <Button variant="ghost" size="sm" onClick={() => void attachments.refetch()}>{t(($) => $.plan_revision.retry)}</Button></p>}
        {attachments.isSuccess && !revisions.length && <p className="px-4 py-2 text-caption text-muted-foreground">{t(($) => $.plan_revision.no_earlier)}</p>}
        {!!revisions.length && !selected && <p className="px-4 py-2 text-caption">{t(($) => $.plan_revision.choose_base)}</p>}
        {current.isPending ? <p role="status" className="p-4 text-body">{t(($) => $.plan_revision.loading)}</p> : current.isError ? <p role="alert" className="p-4 text-body">{t(($) => $.plan_revision.current_error)} <Button variant="outline" size="sm" onClick={() => void current.refetch()}>{t(($) => $.plan_revision.retry)}</Button></p> : <>
          {requested && base.isPending && selected && <p role="status" className="px-4 py-2 text-caption">{t(($) => $.plan_revision.loading_diff)}</p>}
          {requested && base.isError && <p role="alert" className="px-4 py-2 text-caption">{t(($) => $.plan_revision.comparison_error)} <Button variant="outline" size="sm" onClick={() => void base.refetch()}>{t(($) => $.plan_revision.retry)}</Button></p>}
          {comparison?.tooLarge && <p role="alert" className="px-4 py-2 text-caption">{t(($) => $.plan_revision.too_large)}</p>}
          {showingDiff ? <div data-testid="revision-diff" data-mode={mode}>
            <div ref={diffHeader} className="sticky top-0 z-10 flex border-b bg-background px-3 py-1 text-caption text-muted-foreground">
              <span className="min-w-0 flex-1 truncate" title={label(selected!.attachment)}>{t(($) => $.plan_revision.base)}: {label(selected!.attachment)}</span>
              <span className="min-w-0 flex-1 truncate text-right">{t(($) => $.plan_revision.current)}: {attachment.filename}</span>
            </div>
            {!changes && <p role="status" className="p-4 text-body">{t(($) => $.plan_revision.identical)}</p>}
            {(() => {
              let changeIndex = 0;
              return comparison.segments.map((segment, index) => {
                const activeIndex = segment.kind === "change" ? changeIndex++ : null;
                return <section key={index} ref={activeIndex === null ? undefined : (element) => { anchors.current[activeIndex] = element; }} tabIndex={activeIndex === null ? undefined : -1} aria-label={activeIndex === null ? undefined : t(($) => $.plan_revision.change, { current: activeIndex + 1, total: changes })} className={cn("scroll-mt-8", activeIndex === change && "outline-2 -outline-offset-2 outline-ring")}>
                  {segment.kind === "context" && <button type="button" className="w-full border-y bg-muted/40 px-3 py-2 text-left text-caption text-muted-foreground hover:bg-muted" aria-expanded={expanded.has(index)} onClick={() => setExpanded((previous) => { const next = new Set(previous); if (next.has(index)) next.delete(index); else next.add(index); return next; })}>{t(($) => $.plan_revision.unchanged, { count: segment.lines.length })}</button>}
                  {(segment.kind === "change" || expanded.has(index)) && <RevisionRows lines={segment.lines} split={mode === "split"} />}
                </section>;
              });
            })()}
          </div> : <div data-testid="revision-document" className="mx-auto w-full max-w-3xl px-5 py-3 max-[699px]:px-3">
            {current.data?.text === "" ? <p className="text-body text-muted-foreground">{t(($) => $.plan_revision.empty)}</p> : <ReadonlyContent content={current.data!.text} attachments={[attachment]} />}
          </div>}
        </>}
      </main>
      {info?.open && <aside className="w-72 shrink-0 overflow-auto border-l max-[699px]:absolute max-[699px]:inset-y-12 max-[699px]:right-0 max-[699px]:z-20 max-[699px]:bg-background">{info.content}</aside>}
    </div>
  </div>;
}

function DropdownMenuGroupPhone({ mode, setMode, revisions, baseId, chooseBase, onDownload }: {
  mode: string; setMode: (mode: string) => void; revisions: { id: string; label: string }[];
  baseId: string; chooseBase: (id: string) => void; onDownload: () => void;
}) {
  const { t } = useT("editor");
  return <DropdownMenuGroup className="min-[700px]:hidden">
    <DropdownMenuRadioGroup value={mode} onValueChange={setMode}>
      <DropdownMenuRadioItem value="split">{t(($) => $.plan_revision.split)}</DropdownMenuRadioItem>
      <DropdownMenuRadioItem value="unify">{t(($) => $.plan_revision.unify)}</DropdownMenuRadioItem>
    </DropdownMenuRadioGroup>
    <DropdownMenuLabel>{t(($) => $.plan_revision.compare)}</DropdownMenuLabel>
    <DropdownMenuRadioGroup value={baseId} onValueChange={chooseBase}>
      {revisions.map((revision) => <DropdownMenuRadioItem key={revision.id} value={revision.id} className="max-w-80 whitespace-normal break-words text-caption">{revision.label}</DropdownMenuRadioItem>)}
    </DropdownMenuRadioGroup>
    <DropdownMenuItem onClick={onDownload}><ArrowDownToLine className="size-4" />{t(($) => $.plan_revision.download)}</DropdownMenuItem>
  </DropdownMenuGroup>;
}

function RevisionRows({ lines, split }: { lines: RevisionLine[]; split: boolean }) {
  const { t } = useT("editor");
  const kindLabel = (line: RevisionLine | undefined) => line?.kind === "add" ? t(($) => $.plan_revision.added) : line?.kind === "remove" ? t(($) => $.plan_revision.removed) : "";
  const cell = (line: RevisionLine | undefined, side: "old" | "new") => <div className={cn("flex min-w-0 font-mono text-caption leading-6", line?.kind === "add" && "bg-success/10", line?.kind === "remove" && "bg-destructive/10")}>
    <span className="w-[5ch] shrink-0 select-none border-r px-1 text-right text-muted-foreground" aria-hidden="true">{side === "old" ? line?.oldNumber : line?.newNumber}</span>
    <span className="w-[2ch] shrink-0 select-none text-center" aria-hidden="true">{line?.kind === "add" ? "+" : line?.kind === "remove" ? "−" : " "}</span>
    <span className="min-w-0 flex-1 whitespace-pre-wrap break-words px-1 [overflow-wrap:anywhere]"><span className="sr-only">{kindLabel(line)}</span>{line?.text ?? "\u00a0"}</span>
  </div>;
  if (!split) return <>{lines.map((line, index) => <div key={index} className={cn("flex min-w-0 font-mono text-caption leading-6", line.kind === "add" && "bg-success/10", line.kind === "remove" && "bg-destructive/10")}>
    <span className="w-[5ch] shrink-0 select-none border-r px-1 text-right text-muted-foreground" aria-hidden="true">{line.oldNumber}</span>
    <span className="w-[5ch] shrink-0 select-none border-r px-1 text-right text-muted-foreground" aria-hidden="true">{line.newNumber}</span>
    <span className="w-[2ch] shrink-0 select-none text-center" aria-hidden="true">{line.kind === "add" ? "+" : line.kind === "remove" ? "−" : " "}</span>
    <span className="min-w-0 flex-1 whitespace-pre-wrap break-words px-1 [overflow-wrap:anywhere]"><span className="sr-only">{kindLabel(line)}</span>{line.text || "\u00a0"}</span>
  </div>)}</>;
  const rows: { old?: RevisionLine; next?: RevisionLine }[] = [];
  let cursor = 0;
  while (cursor < lines.length) {
    if (lines[cursor]!.kind === "same") { rows.push({ old: lines[cursor], next: lines[cursor++] }); continue; }
    const removed: RevisionLine[] = [];
    const added: RevisionLine[] = [];
    while (cursor < lines.length && lines[cursor]!.kind !== "same") {
      const line = lines[cursor++]!;
      (line.kind === "remove" ? removed : added).push(line);
    }
    for (let index = 0; index < Math.max(removed.length, added.length); index += 1) rows.push({ old: removed[index], next: added[index] });
  }
  return <>{rows.map((row, index) => <div key={index} data-testid="revision-split-row" className="grid grid-cols-2 divide-x">{cell(row.old, "old")}{cell(row.next, "new")}</div>)}</>;
}
