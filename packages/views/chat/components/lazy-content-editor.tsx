"use client";

import { lazy } from "react";

// Tiptap is ~600 KiB; /chat only needs it once the composer mounts, so it
// loads after first paint instead of with the route bundle (CHE-3018).
// test/setup.ts swaps this for the real component so suites stay synchronous.
export const LazyContentEditor = lazy(() =>
  import("../../editor/content-editor").then((m) => ({ default: m.ContentEditor })),
);
