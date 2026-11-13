// Pattern harness (CHE-5428 doop half): renders real @multica/ui and @multica/views components in isolation
// so scripts/design/capture-frame.mjs can screenshot them on X99. Built by run.sh into one inline HTML file
// (headless Chrome on this host cannot reach a local http server, so the page loads over file://).
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

const here = fileURLToPath(new URL(".", import.meta.url));
export default defineConfig({
  root: here,
  plugins: [react()],
  define: { "process.env.NODE_ENV": JSON.stringify("production") },
  resolve: {
    dedupe: ["react", "react-dom", "@tanstack/react-query", "i18next", "react-i18next"],
    // The harness has no workspace; stub the one hook that reads the URL slug.
    alias: [
      // settings-navigation renders the real SettingsPage shell; its tabs and auth-bound hooks are stubbed.
      // These come first: the first matching alias wins, and workspace-stub.ts's paths are a thin subset.
      { find: /^@multica\/core\/paths$/, replacement: resolve(here, "paths-stub.ts") },
      { find: /^@multica\/core\/permissions$/, replacement: resolve(here, "permissions-stub.ts") },
      { find: /^\.{1,2}\/(?:[\w-]+\/)*[\w-]+-tab$/, replacement: resolve(here, "settings-stubs.tsx") },
      ...[
        "@multica/core/hooks",
        "@multica/core/workspace/queries",
        "../../common/actor-avatar",
        "../../workspace/workspace-avatar",
      ].map((id) => ({ find: new RegExp(`^${id.replace(/[/.]/g, "\\$&")}$`), replacement: resolve(here, "workspace-stub.ts") })),
    ],
  },
  build: {
    outDir: process.env.HARNESS_OUT,
    emptyOutDir: true,
    cssCodeSplit: false,
    chunkSizeWarningLimit: 10_000,
    rolldownOptions: { output: { format: "iife", codeSplitting: false } },
  },
});
