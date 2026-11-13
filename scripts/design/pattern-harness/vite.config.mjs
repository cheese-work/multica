// Pattern harness (CHE-5428 doop half): renders real @multica/ui and @multica/views components in isolation
// so scripts/design/capture-frame.mjs can screenshot them on X99. Built by run.sh into one inline HTML file
// (headless Chrome on this host cannot reach a local http server, so the page loads over file://).
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

const here = fileURLToPath(new URL(".", import.meta.url));
const coreHooks = resolve(here, "../../../packages/core/hooks");
export default defineConfig({
  root: here,
  plugins: [
    react(),
    {
      // packages/core mutations import the workspace hook by relative path ("../hooks"); route it to the stub too.
      name: "harness-core-hooks-stub",
      enforce: "pre",
      resolveId(source, importer) {
        if (source.endsWith("/hooks") && importer && resolve(dirname(importer), source) === coreHooks) return resolve(here, "workspace-stub.ts");
        return null;
      },
    },
  ],
  define: { "process.env.NODE_ENV": JSON.stringify("production") },
  resolve: {
    dedupe: ["react", "react-dom", "@tanstack/react-query", "i18next", "react-i18next"],
    // The harness has no workspace, session or websocket: stub the hook that reads the URL slug, the auth
    // store and the realtime barrel (both hang headless Chrome at module init).
    alias: [
      { find: /^@multica\/core\/auth$/, replacement: resolve(here, "auth-stub.ts") },
      { find: /^@multica\/core\/realtime$/, replacement: resolve(here, "realtime-stub.ts") },
      // editor/attachment.tsx imports these by relative path; the real preview modal is a 14 MB graph that hangs headless Chrome.
      { find: /^\.\/attachment-preview-modal$/, replacement: resolve(here, "preview-stub.ts") },
      { find: /^\.\/preview-sequence-context$/, replacement: resolve(here, "sequence-stub.ts") },
      // paths: leaf re-exports, so the F4 components get the path helpers without the barrel.
      { find: /^@multica\/core\/paths$/, replacement: resolve(here, "paths-stub.ts") },
      ...[
        "@multica/core/hooks",
        "@multica/core/workspace/queries",
        "@multica/core/agents",
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
