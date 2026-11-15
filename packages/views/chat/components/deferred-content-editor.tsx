"use client";

import { forwardRef, Suspense, useCallback, useEffect, useImperativeHandle, useRef, useState } from "react";
import { ErrorBoundary } from "@multica/ui/components/common/error-boundary";
import { cn } from "@multica/ui/lib/utils";
import type { ContentEditorProps, ContentEditorRef } from "../../editor/content-editor";
import {
  createLazyContentEditor,
  LazyContentEditor,
  preloadContentEditor,
} from "./lazy-content-editor";

/**
 * Composer editor that costs nothing until it is used (CHE-3018). An empty
 * composer renders a static stand-in that looks like the empty editor; the
 * Tiptap chunk is fetched only on intent (pointer, touch, focus) or when the
 * host needs the editor: a draft or staged attachments exist, or it calls the
 * ref (focus, upload, quote). Calls made before the editor exists are replayed
 * once it is ready.
 *
 * The stand-in is a plain-text editable element that stays mounted until the
 * editor is live, so the caret survives and keys typed while the chunk loads
 * are kept: their text is inserted into the editor, which then takes focus.
 *
 * `draftKey` names the draft the host is composing. Calls that carry content (file, quote, adopted
 * text) are queued with the key they arrived under and dropped if the host has switched drafts by
 * the time the editor is ready, so a file or quote for draft A never lands in draft B.
 */
export type DeferredContentEditorProps = ContentEditorProps & { draftKey?: string };

export const DeferredContentEditor = forwardRef<ContentEditorRef, DeferredContentEditorProps>(
  function DeferredContentEditor({ draftKey, ...props }, ref) {
    const { value, defaultValue, attachments, placeholder, ariaLabel, className, onReady, onUpdate } = props;
    const hasContent = !!(value ?? defaultValue)?.trim() || !!attachments?.length;
    const [wanted, setWanted] = useState(false);
    const [Editor, setEditor] = useState(() => LazyContentEditor);
    const inner = useRef<ContentEditorRef>(null);
    const queue = useRef<Array<{ run: (editor: ContentEditorRef) => void; bound: boolean; draftKey?: string }>>([]);
    const currentDraftKey = useRef(draftKey);
    currentDraftKey.current = draftKey;
    const lastDraftKey = useRef(draftKey);
    const [ready, setReady] = useState(false);
    const [typed, setTyped] = useState(false);
    const typedText = useRef("");
    const standInRef = useRef<HTMLDivElement>(null);
    const retryLoad = useRef<(() => void) | null>(null);

    // Replay after the commit that makes the editor visible: a hidden editor cannot take focus.
    useEffect(() => {
      if (!ready || !inner.current) return;
      // A controlled host already fed the typed text back as `value`, so the editor mounted with it.
      // Insert only into an editor that is still empty (an uncontrolled host).
      const text = typedText.current;
      typedText.current = "";
      if (text.trim() && !inner.current.getText().trim()) inner.current.insertMarkdownAtEnd(text);
      queue.current
        .splice(0)
        .forEach((item) => (!item.bound || item.draftKey === currentDraftKey.current) && item.run(inner.current!));
    }, [ready]);

    // The host owns the draft. When it points the composer at another draft (a session switch) while
    // the stand-in is showing, show that draft's text; never carry the old text across.
    useEffect(() => {
      const switched = lastDraftKey.current !== draftKey;
      lastDraftKey.current = draftKey;
      const el = standInRef.current;
      if (!el || (!switched && (value === undefined || value === typedText.current))) return;
      // A switch without a controlled value still starts from an empty stand-in.
      const next = value ?? "";
      typedText.current = next;
      el.textContent = next;
      setTyped(next.length > 0);
    }, [value, draftKey, ready]);

    // Latch: once mounted the editor stays, even after a send empties the draft.
    if (hasContent && !wanted) setWanted(true);

    // Content-carrying calls pass `bound`: they replay only into the draft they were made for.
    const whenReady = useCallback((run: (editor: ContentEditorRef) => void, bound = false) => {
      if (inner.current) return run(inner.current);
      queue.current.push({ run, bound, draftKey: currentDraftKey.current });
      setWanted(true);
    }, []);

    useImperativeHandle(
      ref,
      (): ContentEditorRef => ({
        getMarkdown: () => inner.current?.getMarkdown() ?? typedText.current,
        getText: () => inner.current?.getText() ?? typedText.current,
        clearContent: () => inner.current?.clearContent(),
        blur: () => inner.current?.blur(),
        hasActiveUploads: () => inner.current?.hasActiveUploads() ?? false,
        flushPendingUpdate: () => inner.current?.flushPendingUpdate() ?? null,
        insertUploadPlaceholder: (upload) => inner.current?.insertUploadPlaceholder(upload) ?? false,
        settleUploadPlaceholder: (id, result) => inner.current?.settleUploadPlaceholder(id, result) ?? false,
        // The stand-in is plain text with no mention chips, so there is nothing to flag before the editor loads.
        flagMentions: (ids) => inner.current?.flagMentions(ids),
        focus: () => whenReady((e) => e.focus()),
        focusAtCoords: (coords) => whenReady((e) => e.focusAtCoords(coords)),
        focusAtAnchor: (anchor) => whenReady((e) => e.focusAtAnchor(anchor)),
        uploadFile: (file) => whenReady((e) => e.uploadFile(file), true),
        adoptContent: (markdown) => {
          if (inner.current || markdown) whenReady((e) => e.adoptContent(markdown), true);
        },
        insertMarkdownAtEnd: (markdown) => {
          if (inner.current) return inner.current.insertMarkdownAtEnd(markdown);
          whenReady((e) => e.insertMarkdownAtEnd(markdown), true);
          return true;
        },
      }),
      [whenReady],
    );

    const wantEditor = () => {
      retryLoad.current?.();
      retryLoad.current = null;
      whenReady((e) => e.focus());
    };

    return (
      <div className="relative flex flex-1 min-h-full flex-col">
        {wanted && (
          // Hidden and out of flow until ready, so the stand-in keeps the layout while the chunk loads.
          <div className={ready ? "contents" : "invisible absolute inset-x-0 top-0"}>
            <ErrorBoundary
              fallback={({ reset }) => (
                // The chunk failed to load: the stand-in stays; the next intent retries.
                <LoadFailed
                  onFailed={() => {
                    retryLoad.current = () => {
                      setEditor(() => createLazyContentEditor());
                      reset();
                    };
                  }}
                />
              )}
            >
              <Suspense fallback={null}>
                <Editor
                  {...props}
                  ref={inner}
                  onReady={() => {
                    onReady?.();
                    setReady(true);
                  }}
                />
              </Suspense>
            </ErrorBoundary>
          </div>
        )}
        {!ready && (
          <div className="relative flex flex-1 flex-col">
            <div
              role="textbox"
              aria-multiline="true"
              aria-label={ariaLabel}
              tabIndex={0}
              contentEditable="plaintext-only"
              suppressContentEditableWarning
              className={cn("flex-1 rich-text-editor text-body outline-none min-h-[1lh] whitespace-pre-wrap", className)}
              onPointerEnter={preloadContentEditor}
              onTouchStart={preloadContentEditor}
              onFocus={() => {
                preloadContentEditor();
                wantEditor();
              }}
              onPointerDown={wantEditor}
              ref={standInRef}
              onInput={(event) => {
                const el = event.currentTarget;
                const text = el.innerText ?? el.textContent ?? "";
                typedText.current = text;
                setTyped(text.length > 0);
                // Hand the text to the host draft now, as the editor would, so it is never held only here.
                onUpdate?.(text, value ?? defaultValue ?? "");
              }}
            />
            {!typed && (
              <span aria-hidden className="pointer-events-none absolute left-0 top-0 text-muted-foreground">
                {placeholder}
              </span>
            )}
          </div>
        )}
      </div>
    );
  },
);

/** Tells the host the editor chunk failed, from an effect rather than during render. */
function LoadFailed({ onFailed }: { onFailed: () => void }) {
  useEffect(onFailed, [onFailed]);
  return null;
}
