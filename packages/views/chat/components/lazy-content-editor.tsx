"use client";

import { lazy } from "react";

const load = () =>
  import("../../editor/content-editor").then((m) => ({ default: m.ContentEditor }));

// Tiptap is ~600 KiB; /chat only needs it once the composer mounts, so it
// loads after first paint instead of with the route bundle (CHE-3018).
// test/setup.ts swaps this for the real component so suites stay synchronous.
// A fresh `lazy` is the only way to retry: React caches a rejected import.
export const createLazyContentEditor = () => lazy(load);
export const LazyContentEditor = createLazyContentEditor();

/** Start fetching the chunk (hover, touch) so a later mount does not wait on the network. */
export const preloadContentEditor = () => {
  load().catch(() => {});
};
