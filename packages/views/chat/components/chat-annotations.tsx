"use client";

import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { MessageSquare, Pencil, X } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@multica/ui/components/ui/popover";
import { useCommentDraftStore, type CommentDraftKey } from "@multica/core/issues/stores";
import type { ReplyAnnotation } from "@multica/core/drafts/reply-annotation";
import { useT } from "../../i18n";
import { visibleRowRange } from "../lib/annotation-range";

/** What the composer needs to show and send the comments of one DM draft. */
export interface ChatInputAnnotations {
  draftKey: CommentDraftKey;
  /** Opens the note at its marker. False when the quoted text is no longer in the reply. */
  onEditAnnotation: (id: string) => boolean;
  activeId?: string | null;
  onActiveChange?: (id: string | null) => void;
}

const focusRing = "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-brand";

/**
 * The pending comments of a DM draft: a count above the composer that opens a
 * scrolling list. Every row keeps its own Edit and Remove button, so all 20
 * comments stay reachable by pointer and keyboard (design rev-2, direction A).
 */
export function ChatAnnotationChip({ draftKey, annotations, disabled, onEditAnnotation, activeId, onActiveChange, onEmpty }: {
  draftKey: CommentDraftKey;
  annotations: ReplyAnnotation[];
  disabled: boolean;
  /** Opens the note at its marker. False when the quoted text is no longer in the reply. */
  onEditAnnotation: (id: string) => boolean;
  activeId?: string | null;
  onActiveChange?: (id: string | null) => void;
  /** Called when the last comment is removed, so the composer can take focus. */
  onEmpty?: () => void;
}) {
  const { t } = useT("chat");
  const [open, setOpen] = useState(false);
  const [unavailable, setUnavailable] = useState<string | null>(null);
  const [announcement, setAnnouncement] = useState("");
  const [range, setRange] = useState<{ from: number; to: number } | null>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  const returningToSource = useRef(false);
  const focusAfterRemove = useRef<number | null>(null);
  const label = t(($) => $.annotations.count, { count: annotations.length });

  const measure = useCallback(() => {
    const area = scrollRef.current;
    if (!area || area.scrollHeight <= area.clientHeight) { setRange(null); return; }
    const rows = Array.from(area.querySelectorAll<HTMLElement>("[data-testid='chat-annotation-row']"))
      .map((row) => ({ top: row.offsetTop, height: row.offsetHeight }));
    setRange(visibleRowRange(rows, area.scrollTop, area.clientHeight));
  }, []);
  // The list mounts in a portal after `open` flips, so measure when the area appears and when rows change.
  const setScrollArea = useCallback((el: HTMLDivElement | null) => {
    scrollRef.current = el;
    if (el) measure();
  }, [measure]);
  useLayoutEffect(() => { if (open) measure(); }, [annotations.length, measure, open]);

  // After Remove, focus the row that took its place (or the one before it).
  useEffect(() => {
    const index = focusAfterRemove.current;
    if (index === null) return;
    focusAfterRemove.current = null;
    const edits = scrollRef.current?.querySelectorAll<HTMLElement>("[data-annotation-edit]");
    edits?.[Math.min(index, edits.length - 1)]?.focus();
  }, [annotations.length]);

  const remove = (index: number) => {
    const annotation = annotations[index]!;
    setAnnouncement(t(($) => $.annotations.removed, { number: index + 1 }));
    if (annotations.length === 1) { setOpen(false); onEmpty?.(); } else focusAfterRemove.current = index;
    onActiveChange?.(null);
    useCommentDraftStore.getState().removeAnnotation(draftKey, annotation.id);
  };

  return <div className="mb-2">
    <span role="status" className="sr-only">{announcement}</span>
    <Popover open={open} onOpenChange={(next) => {
      if (next) returningToSource.current = false;
      else onActiveChange?.(null);
      setOpen(next);
    }}>
      <PopoverTrigger openOnHover delay={150} closeDelay={200} disabled={disabled}
        data-testid="chat-annotation-chip"
        render={<Button variant="outline" size="sm" className={focusRing} />}>
        <MessageSquare />{label}
      </PopoverTrigger>
      <PopoverContent side="top" align="start" aria-label={label}
        className="w-[380px] max-w-[calc(100vw-16px)] gap-1 p-2"
        initialFocus={false}
        finalFocus={() => !returningToSource.current}
        onClick={(event) => event.stopPropagation()}>
        <div ref={setScrollArea} data-testid="chat-annotation-scroll" role="region" tabIndex={0}
          aria-label={t(($) => $.annotations.list_label)}
          onScroll={measure}
          // `relative`: rows measure their offsetTop against this area, not the popup padding around it.
          className={`relative flex max-h-[272px] flex-col gap-2 overflow-y-auto rounded-md pr-2 max-md:max-h-[248px] ${focusRing}`}>
          {annotations.map((annotation, index) => <div key={annotation.id}
            data-testid="chat-annotation-row" data-index={index} data-active={activeId === annotation.id}
            onMouseEnter={() => onActiveChange?.(annotation.id)} onMouseLeave={() => onActiveChange?.(null)}
            onFocus={() => onActiveChange?.(annotation.id)}
            onBlur={(event) => { if (!event.currentTarget.contains(event.relatedTarget)) onActiveChange?.(null); }}
            className="flex items-start gap-1 rounded-md p-0.5 data-[active=true]:bg-muted">
            <span aria-hidden="true" className="mt-1 flex size-6 shrink-0 items-center justify-center rounded-full border border-brand/28 bg-brand/7 text-caption tabular-nums">{index + 1}</span>
            <div className="min-w-0 flex-1 p-1 text-body">
              <blockquote className="max-h-28 overflow-y-auto whitespace-pre-wrap break-words border-l-2 border-border pl-2 text-muted-foreground">{annotation.quote}</blockquote>
              <p className="mt-1 whitespace-pre-wrap break-words">{annotation.note}</p>
              {unavailable === annotation.id && <p role="status" className="mt-0.5 text-caption text-muted-foreground">
                {t(($) => $.annotations.source_changed)}
              </p>}
            </div>
            <Button variant="ghost" size="icon-sm" data-testid="chat-annotation-edit" data-annotation-edit
              className={focusRing}
              aria-label={t(($) => $.annotations.edit_row, { number: index + 1 })}
              onFocus={(event) => event.currentTarget.scrollIntoView?.({ block: "nearest" })}
              onClick={() => {
                if (onEditAnnotation(annotation.id)) {
                  returningToSource.current = true;
                  setOpen(false); setUnavailable(null);
                } else setUnavailable(annotation.id);
              }}><Pencil /></Button>
            <Button variant="ghost" size="icon-sm" data-testid="chat-annotation-remove" className={focusRing}
              aria-label={t(($) => $.annotations.remove, { number: index + 1 })}
              onClick={() => remove(index)}><X /></Button>
          </div>)}
        </div>
        {range && <p data-testid="chat-annotation-range" aria-live="polite" className="px-1 pt-1 text-caption tabular-nums text-muted-foreground">
          {t(($) => $.annotations.range, { from: range.from, to: range.to, total: annotations.length })}
        </p>}
      </PopoverContent>
    </Popover>
  </div>;
}
