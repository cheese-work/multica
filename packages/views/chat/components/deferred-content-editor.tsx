"use client";

import { forwardRef, Suspense, useCallback, useImperativeHandle, useRef, useState } from "react";
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
 * Keys typed in the window between focus and the chunk arriving are lost;
 * hover/touch prefetch makes that window small. ponytail: buffer and replay
 * them if real users hit it.
 */
export const DeferredContentEditor = forwardRef<ContentEditorRef, ContentEditorProps>(
  function DeferredContentEditor(props, ref) {
    const { value, defaultValue, attachments, placeholder, ariaLabel, className, onReady } = props;
    const hasContent = !!(value ?? defaultValue)?.trim() || !!attachments?.length;
    const [wanted, setWanted] = useState(false);
    const [Editor, setEditor] = useState(() => LazyContentEditor);
    const inner = useRef<ContentEditorRef>(null);
    const queue = useRef<Array<(editor: ContentEditorRef) => void>>([]);

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
        getMarkdown: () => inner.current?.getMarkdown() ?? "",
        getText: () => inner.current?.getText() ?? "",
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

    const standIn = (onIntent: () => void) => (
      <div
        role="textbox"
        aria-multiline="true"
        aria-label={ariaLabel}
        tabIndex={0}
        className={cn("flex-1 rich-text-editor text-body outline-none", className)}
        onPointerEnter={preloadContentEditor}
        onTouchStart={preloadContentEditor}
        onFocus={() => {
          preloadContentEditor();
          onIntent();
        }}
        onPointerDown={onIntent}
      >
        <p className="is-editor-empty" data-placeholder={placeholder}>
          <br />
        </p>
      </div>
    );
    const wantEditor = () => whenReady((e) => e.focus());

    if (!wanted) return <div className="relative flex flex-1 min-h-full flex-col">{standIn(wantEditor)}</div>;
    return (
      <ErrorBoundary
        fallback={({ reset }) =>
          // The chunk failed to load: show the composer again; the next intent retries.
          standIn(() => {
            setEditor(() => createLazyContentEditor());
            reset();
            wantEditor();
          })
        }
      >
        <Suspense fallback={standIn(wantEditor)}>
          <Editor
            {...props}
            ref={inner}
            onReady={() => {
              onReady?.();
              const pending = queue.current.splice(0);
              if (inner.current) pending.forEach((run) => run(inner.current!));
            }}
          />
        </Suspense>
      </ErrorBoundary>
    );
  },
);
