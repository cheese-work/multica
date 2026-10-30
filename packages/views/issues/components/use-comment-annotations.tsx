"use client";

import { useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type RefObject } from "react";
import { MessageSquarePlus, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { isImeComposing } from "@multica/core/utils";
import { Button } from "@multica/ui/components/ui/button";
import { IconButton } from "@multica/ui/components/common/icon-button";
import { useCommentDraftStore, type CommentDraftKey } from "@multica/core/issues/stores";
import { MAX_ANNOTATION_QUOTE_LENGTH, MAX_REPLY_ANNOTATIONS, type ReplyAnnotation } from "@multica/core/drafts/reply-annotation";
import { useT } from "../../i18n";
import { annotationRange, captureCommentSelection, findAnnotationSource } from "./comment-annotation-selection";
import { CommentSelectionBubble } from "./comment-selection-bubble";

/** Text overrides for a surface that is not an issue thread (the DM). */
export interface AnnotationLabels {
  selectionFailed: string;
  quoteLimit: (count: number) => string;
  countLimit: (count: number) => string;
  add: string;
  confirmAdd: string;
  save: string;
  noteLabel: (number: number) => string;
  notePlaceholder: string;
  edit: (number: number, note: string) => string;
  remove: (number: number) => string;
}

/** A chat message renders its markers in its own slot so Tab reaches them in reading order. */
function overlaySlot(root: HTMLElement): HTMLElement | undefined {
  return root.closest("[data-annotation-overlay-host]")?.querySelector<HTMLElement>(":scope > [data-annotation-overlay-slot]") ?? undefined;
}

/**
 * The note keeps its own text: a keystroke (and every IME composition update)
 * re-renders only this editor, never the chat or issue page that hosts the hook.
 */
function AnnotationNoteEditor({ inputRef, initialNote, label, showLabel, placeholder, confirmLabel, removeLabel, onEdit, onConfirm, onCancel, onRemove }: {
  inputRef: RefObject<HTMLTextAreaElement | null>;
  initialNote: string;
  label: string;
  showLabel: boolean;
  placeholder: string;
  confirmLabel: string;
  removeLabel?: string;
  onEdit: (note: string) => void;
  onConfirm: (note: string) => void;
  onCancel: () => void;
  onRemove: () => void;
}) {
  const [note, setNote] = useState(initialNote);
  useLayoutEffect(() => {
    const input = inputRef.current;
    if (!input) return;
    input.style.height = "auto";
    input.style.height = `${input.scrollHeight}px`;
  }, [note, inputRef]);
  return <div className="flex w-80 max-w-[calc(100vw-26px)] flex-col gap-1">
    {showLabel && <p className="px-2 pt-1 text-caption text-muted-foreground">{label}</p>}
    <textarea ref={inputRef} value={note} rows={1}
      aria-label={label}
      data-testid="annotation-note"
      placeholder={placeholder}
      className="min-h-8 max-h-[calc(4lh+0.75rem)] w-full min-w-0 resize-none overflow-y-auto rounded-sm bg-transparent px-2 py-1.5 text-body outline-none placeholder:text-muted-foreground"
      onChange={(event) => { setNote(event.target.value); onEdit(event.target.value); }}
      onKeyDown={(event) => {
        if (event.key === "Enter" && !isImeComposing(event) &&
          !event.shiftKey && !event.metaKey && !event.ctrlKey && !event.altKey) {
          event.preventDefault(); event.stopPropagation(); onConfirm(note);
        } else if (event.key === "Escape" && !isImeComposing(event)) {
          event.preventDefault(); event.stopPropagation(); onCancel();
        }
      }} />
    <div className="flex items-center justify-end gap-1 px-1 pb-1">
      {removeLabel && <IconButton variant="ghost" size="icon-sm" className="mr-auto text-muted-foreground hover:text-destructive"
        label={removeLabel} onClick={onRemove}><Trash2 /></IconButton>}
      <Button size="sm" disabled={!note.trim()}
        aria-label={confirmLabel} aria-keyshortcuts="Enter"
        onClick={() => onConfirm(note)}>{confirmLabel}</Button>
    </div>
  </div>;
}

type CapturedSelection = NonNullable<ReturnType<typeof captureCommentSelection>>;
type SourceAnchor = { id: string; range: Range; root: HTMLElement };

export function useCommentAnnotations({ draftKey, sources, enabled, onAdded, onRevealSource, editable = false, surface = "issue", labels }: {
  draftKey: CommentDraftKey;
  sources: { id: string; name: string; revision?: number }[];
  enabled: boolean;
  onAdded?: () => void;
  onRevealSource?: (annotation: ReplyAnnotation) => boolean;
  /** Editable descriptions use their existing toolbar to call addSelection. */
  editable?: boolean;
  /** `chat` renders markers inside the message (reading order) and returns focus to them. */
  surface?: "issue" | "chat";
  labels?: AnnotationLabels;
}) {
  const { t } = useT("issues");
  const chat = surface === "chat";
  const L: AnnotationLabels = labels ?? {
    selectionFailed: t(($) => $.reply.annotations.selection_failed),
    quoteLimit: (count) => t(($) => $.reply.annotations.quote_limit, { count }),
    countLimit: (count) => t(($) => $.reply.annotations.count_limit, { count }),
    add: t(($) => editable ? $.reply.annotations.add_comment : $.reply.annotations.add),
    confirmAdd: t(($) => $.reply.annotations.confirm_add),
    save: t(($) => $.reply.annotations.save),
    noteLabel: () => t(($) => $.reply.annotations.note_label),
    notePlaceholder: t(($) => $.reply.annotations.note_placeholder),
    edit: (number) => t(($) => $.reply.annotations.edit, { number }),
    remove: (number) => t(($) => $.reply.annotations.remove, { number }),
  };
  const Lref = useRef(L);
  Lref.current = L;
  const [activeId, setActiveId] = useState<string | null>(null);
  const pendingMarkerFocus = useRef<string | null>(null);
  const cardRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const pendingFocusId = useRef<string | null>(null);
  const actionRef = useRef<HTMLButtonElement>(null);
  const [selection, setSelection] = useState<CapturedSelection | null>(null);
  const [editor, setEditor] = useState<{ annotation: ReplyAnnotation; existing: boolean } | null>(null);
  const editing = editor?.annotation;
  const editingExisting = editor?.existing === true;
  const editingRef = useRef(editing);
  editingRef.current = editing;
  const noteDrafts = useRef(new Map<string, { note: string; savedNote: string }>());
  const noteDraftKey = useCallback((annotation: ReplyAnnotation) => JSON.stringify([
    draftKey, annotation.sourceCommentId, annotation.start, annotation.quote, annotation.prefix, annotation.suffix,
  ]), [draftKey]);
  const editingId = editing?.id;
  const [error, setError] = useState(false);
  const [anchors, setAnchors] = useState<SourceAnchor[]>([]);
  const markerRanges = useMemo(() => {
    const groups = new Map<HTMLElement, Range[]>();
    for (const anchor of anchors) {
      const group = groups.get(anchor.root) ?? [];
      group.push(anchor.range); groups.set(anchor.root, group);
    }
    return groups;
  }, [anchors]);
  const annotations = useCommentDraftStore((s) => s.getAnnotations(draftKey));
  const annotationsRef = useRef(annotations);
  annotationsRef.current = annotations;
  const sourcesRef = useRef(sources);
  sourcesRef.current = sources;
  const onAddedRef = useRef(onAdded);
  onAddedRef.current = onAdded;
  const anchorKey = annotations.map((a) => a.id).join(",");
  const highlightName = `reply-annotation-${useId().replace(/[^a-zA-Z0-9-]/g, "")}`;
  const editorOpenEvent = `reply-annotation-open:${draftKey}`;
  const claimEditor = useCallback(() => {
    document.dispatchEvent(new CustomEvent(editorOpenEvent, { detail: highlightName }));
  }, [editorOpenEvent, highlightName]);

  const findSource = useCallback((sourceCommentId: string) => {
    const card = cardRef.current;
    if (!card) return undefined;
    const local = findAnnotationSource(card, sourceCommentId);
    if (local) return local;
    const timeline = card.closest("[data-issue-timeline-scroll]");
    if (!timeline) return undefined;
    for (const scope of timeline.querySelectorAll<HTMLElement>("[data-annotation-thread]")) {
      if (scope.dataset.annotationThread !== draftKey) continue;
      const source = findAnnotationSource(scope, sourceCommentId);
      if (source) return source;
    }
    return undefined;
  }, [draftKey]);

  const markerFor = (id: string) =>
    cardRef.current?.querySelector<HTMLElement>(`[data-annotation-marker="${id}"]`) ?? null;
  const close = (restoreFocus = false) => {
    const marker = restoreFocus && chat && editingId ? markerFor(editingId) : null;
    if (marker) marker.focus({ preventScroll: true });
    else if (restoreFocus && selection?.root.isConnected) {
      const target = editable ? selection.root.querySelector<HTMLElement>("[contenteditable=true]") ?? selection.root : selection.root;
      target.focus({ preventScroll: true });
    }
    setSelection(null); setEditor(null); setError(false);
  };
  const readSelection = useCallback(() => {
    if (!enabled || !cardRef.current) return null;
    const captured = captureCommentSelection(cardRef.current, window.getSelection(), editable);
    return captured && sourcesRef.current.some((source) => source.id === captured.sourceCommentId) ? captured : null;
  }, [enabled, editable]);
  const capture = () => {
    const captured = readSelection();
    if (captured) { pendingMarkerFocus.current = null; claimEditor(); setSelection(captured); setEditor(null); setError(false); }
  };

  useEffect(() => { noteDrafts.current.clear(); setSelection(null); setEditor(null); setError(false); }, [draftKey, enabled]);

  // A drag can end anywhere (padding, gutter, another element), so a press inside the message arms
  // a document-level pointerup; captureCommentSelection then decides from the selection alone.
  const armed = useRef(false);
  const captureRef = useRef(capture);
  captureRef.current = capture;
  useEffect(() => {
    const onUp = () => { if (armed.current) { armed.current = false; captureRef.current(); } };
    const disarm = () => { armed.current = false; };
    document.addEventListener("pointerup", onUp);
    document.addEventListener("pointercancel", disarm);
    return () => { document.removeEventListener("pointerup", onUp); document.removeEventListener("pointercancel", disarm); };
  }, []);

  // Dismiss on the next press, never on the click completing the opening drag.
  useEffect(() => {
    if (!selection) return;
    let touch: { id: number; x: number; y: number; moved: boolean } | null = null;
    const onEditorOpen = (event: Event) => {
      if (!(event instanceof CustomEvent) || event.detail === highlightName) return;
      setSelection(null); setEditor(null); setError(false);
    };
    const onPointerDown = (event: PointerEvent) => {
      if (event.target instanceof Element &&
        event.target.closest("[data-reply-annotation-overlay]")?.getAttribute("data-reply-annotation-overlay") === draftKey) return;
      if (event.pointerType === "touch") {
        touch = { id: event.pointerId, x: event.clientX, y: event.clientY, moved: false };
        return;
      }
      setSelection(null); setEditor(null); setError(false);
    };
    const onPointerMove = (event: PointerEvent) => {
      if (touch?.id === event.pointerId && Math.hypot(event.clientX - touch.x, event.clientY - touch.y) > 8) touch.moved = true;
    };
    const onPointerUp = (event: PointerEvent) => {
      if (touch?.id !== event.pointerId) return;
      onPointerMove(event);
      const dismiss = !touch.moved;
      touch = null;
      if (dismiss) { setSelection(null); setEditor(null); setError(false); }
    };
    const onPointerCancel = () => { touch = null; };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || isImeComposing(event)) return;
      if (editingRef.current) noteDrafts.current.delete(noteDraftKey(editingRef.current));
      if (selection.root.isConnected) {
        const target = editable ? selection.root.querySelector<HTMLElement>("[contenteditable=true]") ?? selection.root : selection.root;
        target.focus({ preventScroll: true });
      }
      setSelection(null); setEditor(null); setError(false);
    };
    const onFocusIn = (event: FocusEvent) => {
      if (touch || !(event.target instanceof Element) || selection.root.contains(event.target) ||
        event.target.closest("[data-reply-annotation-overlay]")?.getAttribute("data-reply-annotation-overlay") === draftKey) return;
      setSelection(null); setEditor(null); setError(false);
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("pointermove", onPointerMove);
    document.addEventListener("pointerup", onPointerUp);
    document.addEventListener("pointercancel", onPointerCancel);
    document.addEventListener("keydown", onKeyDown);
    document.addEventListener("focusin", onFocusIn);
    document.addEventListener(editorOpenEvent, onEditorOpen);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("pointermove", onPointerMove);
      document.removeEventListener("pointerup", onPointerUp);
      document.removeEventListener("pointercancel", onPointerCancel);
      document.removeEventListener("keydown", onKeyDown);
      document.removeEventListener("focusin", onFocusIn);
      document.removeEventListener(editorOpenEvent, onEditorOpen);
    };
  }, [selection, draftKey, editable, editorOpenEvent, highlightName, noteDraftKey]);

  const focusNote = useCallback(() => {
    if (!editingId || pendingFocusId.current !== editingId) return;
    pendingFocusId.current = null;
    if (window.matchMedia("(pointer: fine)").matches) inputRef.current?.focus({ preventScroll: true });
  }, [editingId]);

  useEffect(() => {
    if ((!anchorKey && !editingId) || !enabled) {
      setAnchors((current) => current.length ? [] : current);
      return;
    }
    if (!cardRef.current) return;
    const card = cardRef.current;
    const canHighlight = typeof Highlight !== "undefined" && typeof CSS !== "undefined" && !!CSS.highlights;
    const update = () => {
      const next = annotationsRef.current.flatMap((a) => {
        const root = findAnnotationSource(card, a.sourceCommentId);
        const range = root && annotationRange(root, a, editable);
        return root && range ? [{ id: a.id, range, root }] : [];
      });
      setAnchors((current) => !current.length && !next.length ? current : next);
      // Expanding a resolved thread may remount the selected comment's body.
      // Keep the note editor attached to its saved quote, never a detached Range.
      setSelection((current) => {
        if (!current) return current;
        const saved = annotationsRef.current.find((a) => a.sourceCommentId === current.sourceCommentId &&
          a.start === current.start && a.quote === current.quote);
        const anchor = saved && next.find((a) => a.id === saved.id);
        if (anchor) return { ...current, root: anchor.root, range: anchor.range };
        const pending = saved ?? editingRef.current;
        if (pending?.sourceCommentId === current.sourceCommentId) {
          const root = findSource(pending.sourceCommentId);
          const range = root && annotationRange(root, pending, editable);
          return root && range ? { ...current, root, range } : null;
        }
        if (saved) return null;
        return current.root.isConnected && current.range.startContainer.isConnected ? current : null;
      });
      if (canHighlight) CSS.highlights.set(highlightName, new Highlight(...next.map((a) => a.range)));
    };
    update();
    const observer = new MutationObserver((records) => {
      // Reply typing must not re-index a long source comment.
      if (records.some((record) => {
        const target = record.target instanceof Element ? record.target : record.target.parentElement;
        return target?.closest("[data-comment-content]") ||
          [...record.addedNodes, ...record.removedNodes].some((node) => node instanceof Element &&
            (node.matches("[data-comment-content]") || node.querySelector("[data-comment-content]")));
      })) update();
    });
    observer.observe(editingId ? card.closest("[data-issue-timeline-scroll]") ?? card : card,
      { childList: true, subtree: true, characterData: true });
    return () => { observer.disconnect(); if (canHighlight) CSS.highlights.delete(highlightName); };
  }, [anchorKey, highlightName, draftKey, editable, enabled, editingId, findSource]);

  useEffect(() => {
    if (!selection || (editor?.existing && !annotations.some((a) => a.id === editingId))) {
      setSelection(null); setEditor(null);
    }
  }, [annotations, editor?.existing, editingId, selection]);

  const editAnnotation = (id: string, scrollToSource = false): boolean => {
    const annotation = annotationsRef.current.find((a) => a.id === id);
    const root = annotation && findSource(annotation.sourceCommentId);
    if (annotation && !root) return onRevealSource?.(annotation) ?? false;
    const range = root && annotation && annotationRange(root, annotation, editable);
    if (!root || !range || !annotation) return false;
    pendingMarkerFocus.current = null;
    if (scrollToSource) {
      const element = range.startContainer instanceof Element ? range.startContainer : range.startContainer.parentElement;
      element?.scrollIntoView({ block: "center", behavior: "instant" });
    }
    claimEditor();
    pendingFocusId.current = id;
    setSelection({ ...annotation, range, root }); setEditor({ annotation, existing: true }); setError(false);
    return true;
  };

  const add = useCallback((captured: CapturedSelection | null) => {
    const source = captured && sourcesRef.current.find((e) => e.id === captured.sourceCommentId);
    if (!captured || !source) {
      toast.error(Lref.current.selectionFailed);
      return false;
    }
    const { quote, start, prefix, suffix, sourceCommentId } = captured;
    const duplicate = annotationsRef.current.find((a) => a.sourceCommentId === sourceCommentId &&
      a.start === start && a.quote === quote && a.prefix === prefix && a.suffix === suffix);
    if (quote.length > MAX_ANNOTATION_QUOTE_LENGTH || (!duplicate && annotationsRef.current.length >= MAX_REPLY_ANNOTATIONS)) {
      setError(true);
      toast.error(quote.length > MAX_ANNOTATION_QUOTE_LENGTH
        ? Lref.current.quoteLimit(MAX_ANNOTATION_QUOTE_LENGTH)
        : Lref.current.countLimit(MAX_REPLY_ANNOTATIONS));
      return false;
    }
    pendingMarkerFocus.current = null;
    claimEditor();
    setSelection(captured);
    const next = duplicate ?? {
      id: crypto.randomUUID(), sourceCommentId, sourceActorName: source.name,
      sourceRevision: source.revision, quote, start, prefix, suffix, note: "",
    };
    pendingFocusId.current = next.id;
    setEditor({ annotation: next, existing: !!duplicate });
    setError(false);
    window.getSelection()?.removeAllRanges();
    return true;
  }, [claimEditor]);
  const addSelection = useCallback(() => add(readSelection()), [add, readSelection]);

  const confirmNote = (note: string) => {
    if (!editing || !note.trim()) return;
    const store = useCommentDraftStore.getState();
    if (editingExisting) store.updateAnnotation(draftKey, editing.id, note);
    else if (!store.addAnnotation(draftKey, { ...editing, note })) {
      setError(true);
      return;
    }
    noteDrafts.current.delete(noteDraftKey(editing));
    if (chat && !editingExisting) { pendingMarkerFocus.current = editing.id; close(false); }
    else close(true);
    if (!editingExisting) onAddedRef.current?.();
  };

  // Keep the pending quote identifiable without counting it as an annotation.
  useEffect(() => {
    if (!enabled || !selection || !editingId || typeof Highlight === "undefined" ||
      typeof CSS === "undefined" || !CSS.highlights) return;
    const name = `${highlightName}-selection`;
    CSS.highlights.set(name, new Highlight(selection.range));
    return () => { CSS.highlights.delete(name); };
  }, [enabled, selection, editingId, highlightName]);
  const editingNumber = editing ? annotations.findIndex((a) => a.id === editing.id) + 1 || annotations.length + 1 : 1;
  // The pending quote and the active comment (hovered or focused marker, a lit list row, or the open editor).
  useEffect(() => {
    if (!enabled || typeof Highlight === "undefined" || typeof CSS === "undefined" || !CSS.highlights) return;
    const name = `${highlightName}-active`;
    const anchor = anchors.find((a) => a.id === (activeId ?? editingId));
    if (!anchor) { CSS.highlights.delete(name); return; }
    CSS.highlights.set(name, new Highlight(anchor.range));
    return () => { CSS.highlights.delete(name); };
  }, [enabled, anchors, activeId, editingId, highlightName]);
  const confirmLabel = editor?.existing ? L.save : L.confirmAdd;
  const noteDraft = editing && noteDrafts.current.get(noteDraftKey(editing));

  return {
    cardRef,
    editAnnotation,
    activeId,
    setActiveId,
    addSelection,
    captureProps: {
      "data-annotation-thread": draftKey,
      onPointerDown: (event: React.PointerEvent) => {
        armed.current = !editable && event.target instanceof Element && !!event.target.closest("[data-comment-content]") &&
          !event.target.closest("button, [contenteditable=true]");
      },
      onKeyUp: (event: React.KeyboardEvent) => {
        if (!editable && event.shiftKey && event.key.startsWith("Arrow")) capture();
      },
      onKeyDown: (event: React.KeyboardEvent) => {
        if (event.key === "Tab" && !event.shiftKey && selection && !editingId &&
          event.target instanceof Element && event.target.closest("[data-comment-content]")) {
          event.preventDefault(); actionRef.current?.focus();
        }
      },
    },
    popup: <>
      {(annotations.length > 0 || editing) && <style>{`::highlight(${highlightName}), ::highlight(${highlightName}-selection) { background: color-mix(in srgb, var(--brand) 20%, transparent); } ::highlight(${highlightName}-active) { background: color-mix(in srgb, var(--brand) 38%, transparent); }`}</style>}
      {enabled && anchors.map((anchor) => {
        const index = annotations.findIndex((a) => a.id === anchor.id);
        if (index < 0) return null;
        return <CommentSelectionBubble key={anchor.id} range={anchor.range} source={anchor.root} owner={draftKey}
          markerRanges={markerRanges.get(anchor.root)}
          portalTarget={chat ? overlaySlot(anchor.root) : undefined}
          onPositioned={() => {
            if (pendingMarkerFocus.current !== anchor.id) return;
            pendingMarkerFocus.current = null;
            markerFor(anchor.id)?.focus({ preventScroll: true });
          }}>
          <IconButton variant="brandSubtle" size="icon-sm"
            aria-expanded={editingId === anchor.id}
            label={L.edit(index + 1, annotations[index]?.note ?? "")}
            data-testid="annotation-marker" data-annotation-marker={anchor.id} data-active={activeId === anchor.id || editingId === anchor.id}
            className="size-6 rounded-full text-caption tabular-nums aria-expanded:ring-2 aria-expanded:ring-brand data-[active=true]:ring-2 data-[active=true]:ring-brand focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-brand"
            onMouseEnter={() => setActiveId(anchor.id)} onMouseLeave={() => setActiveId((id) => id === anchor.id ? null : id)}
            onFocus={() => setActiveId(anchor.id)} onBlur={() => setActiveId((id) => id === anchor.id ? null : id)}
            onClick={() => editAnnotation(anchor.id)}>{index + 1}</IconButton>
        </CommentSelectionBubble>;
      })}
      {enabled && selection && <CommentSelectionBubble range={selection.range} source={selection.root} owner={draftKey} onPositioned={focusNote} keepInView={!!editing}>
        <div className="bubble-menu max-w-[calc(100vw-16px)] focus-within:outline-1 focus-within:outline-ring">
          {editing ? <AnnotationNoteEditor key={editing.id} inputRef={inputRef} initialNote={noteDraft?.savedNote === editing.note ? noteDraft.note : editing.note}
              label={L.noteLabel(editingNumber)} showLabel={chat && editingExisting} placeholder={L.notePlaceholder}
              confirmLabel={confirmLabel} removeLabel={editingExisting ? L.remove(editingNumber) : undefined}
              onEdit={(note) => { noteDrafts.current.set(noteDraftKey(editing), { note, savedNote: editing.note }); if (error) setError(false); }} onConfirm={confirmNote}
              onCancel={() => { noteDrafts.current.delete(noteDraftKey(editing)); close(true); }}
              onRemove={() => {
                noteDrafts.current.delete(noteDraftKey(editing));
                useCommentDraftStore.getState().removeAnnotation(draftKey, editing.id);
                close(true);
              }} /> : <Button ref={actionRef} variant="ghost" size="sm" data-testid="annotation-add"
              onMouseDown={(event) => event.preventDefault()} onClick={() => add(selection)}>
              <MessageSquarePlus />{L.add}
            </Button>}
        </div>
        {error && <p role="alert" className="mt-1 max-w-72 rounded-lg bg-popover p-2 text-caption text-destructive shadow-[var(--menu-shadow)]">
          {selection.quote.length > MAX_ANNOTATION_QUOTE_LENGTH
            ? L.quoteLimit(MAX_ANNOTATION_QUOTE_LENGTH)
            : L.countLimit(MAX_REPLY_ANNOTATIONS)}
        </p>}
      </CommentSelectionBubble>}
    </>,
  };
}
