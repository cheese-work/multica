"use client";

/**
 * Barrel-facing ContentEditor / TitleEditor. The Tiptap + ProseMirror graph
 * (~100 KB gz, evaluated on the main thread) loads on first render instead of
 * sitting in every route that imports `editor/index.ts`, so readonly-first
 * hosts paint their markdown before the editor stack is parsed.
 */

import { forwardRef, lazy, Suspense, type ForwardRefExoticComponent, type RefAttributes } from "react";
import type { ContentEditorProps, ContentEditorRef } from "./content-editor";
import type { TitleEditorProps, TitleEditorRef } from "./title-editor";
import { ReadonlyContent } from "./readonly-content";

type ContentEditorType = ForwardRefExoticComponent<ContentEditorProps & RefAttributes<ContentEditorRef>>;
type TitleEditorType = ForwardRefExoticComponent<TitleEditorProps & RefAttributes<TitleEditorRef>>;

// Once a chunk has loaded, later mounts skip Suspense and render synchronously.
let loadedContent: ContentEditorType | null = null;
let loadedTitle: TitleEditorType | null = null;

const loadContent = () =>
  import("./content-editor").then((m) => {
    loadedContent = m.ContentEditor;
    return { default: m.ContentEditor };
  });
const loadTitle = () =>
  import("./title-editor").then((m) => {
    loadedTitle = m.TitleEditor;
    return { default: m.TitleEditor };
  });
const LazyContent = lazy(loadContent);
const LazyTitle = lazy(loadTitle);

/** Warm both chunks (tests await this so the first render mounts synchronously). */
export const preloadEditors = async () => {
  await Promise.all([loadContent(), loadTitle()]);
};

export const ContentEditor = forwardRef<ContentEditorRef, ContentEditorProps>(
  function ContentEditor(props, ref) {
    if (loadedContent) {
      const Loaded = loadedContent;
      return <Loaded ref={ref} {...props} />;
    }
    // The description must not wait for the editor chunk: show it read-only.
    const text = props.value ?? props.defaultValue;
    return (
      <Suspense fallback={text ? <ReadonlyContent content={text} /> : null}>
        <LazyContent ref={ref} {...props} />
      </Suspense>
    );
  },
);

export const TitleEditor = forwardRef<TitleEditorRef, TitleEditorProps>(
  function TitleEditor(props, ref) {
    if (loadedTitle) {
      const Loaded = loadedTitle;
      return <Loaded ref={ref} {...props} />;
    }
    return (
      <Suspense fallback={props.defaultValue ?? null}>
        <LazyTitle ref={ref} {...props} />
      </Suspense>
    );
  },
);
