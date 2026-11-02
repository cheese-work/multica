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
 */
export const DeferredContentEditor = forwardRef<ContentEditorRef, ContentEditorProps>(
  function DeferredContentEditor(props, ref) {
    const { value, defaultValue, attachments, placeholder, ariaLabel, className, onReady, onUpdate } = props;
    const hasContent = !!(value ?? defaultValue)?.trim() || !!attachments?.length;
    const [wanted, setWanted] = useState(false);
    const [Editor, setEditor] = useState(() => LazyContentEditor);
    const inner = useRef<ContentEditorRef>(null);
    const queue = useRef<Array<(editor: ContentEditorRef) => void>>([]);
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
      queue.current.splice(0).forEach((run) => run(inner.current!));
    }, [ready]);

    // The host owns the draft. When it points the composer at another draft (a session switch) while
    // the stand-in is showing, show that draft's text; never carry the old text across.
    useEffect(() => {
      const el = standInRef.current;
      if (!el || value === undefined || value === typedText.current) return;
      typedText.current = value;
      el.textContent = value;
      setTyped(value.length > 0);
    }, [value, ready]);

    // Latch: once mounted the editor stays, even after a send empties the draft.
    if (hasContent && !wanted) setWanted(true);

    const whenReady = useCallback((run: (editor: ContentEditorRef) => void) => {
      if (inner.current) return run(inner.current);
      queue.current.push(run);
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
        focus: () => whenReady((e) => e.focus()),
        focusAtCoords: (coords) => whenReady((e) => e.focusAtCoords(coords)),
        focusAtAnchor: (anchor) => whenReady((e) => e.focusAtAnchor(anchor)),
        uploadFile: (file) => whenReady((e) => e.uploadFile(file)),
        adoptContent: (markdown) => {
          if (inner.current || markdown) whenReady((e) => e.adoptContent(markdown));
        },
        insertMarkdownAtEnd: (markdown) => {
          if (inner.current) return inner.current.insertMarkdownAtEnd(markdown);
          whenReady((e) => e.insertMarkdownAtEnd(markdown));
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
