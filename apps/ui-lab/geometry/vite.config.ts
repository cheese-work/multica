import { resolve } from "node:path";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

const repo = resolve(__dirname, "../../..");

export default defineConfig({
  root: __dirname,
  // The harness imports the app packages by relative path and its
  // dependencies from the workspace install. Keep Vite's file-serving
  // boundary on and allow only those trees, not the whole repo or disk.
  plugins: [react(), tailwindcss()],
  server: {
    fs: {
      strict: true,
      allow: [resolve(repo, "apps/ui-lab"), resolve(repo, "packages"), resolve(repo, "node_modules")],
    },
  },
});
