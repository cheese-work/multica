import { resolve } from "node:path";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// Run from apps/desktop (see ../README.md): its node_modules hold the build tools and React.
const repo = resolve(__dirname, "../../../..");
const desktop = resolve(repo, "apps/desktop/node_modules");
export default {
  root: __dirname,
  plugins: [react(), tailwindcss()],
  server: { fs: { strict: false } },
  build: { outDir: resolve(__dirname, "dist"), emptyOutDir: true },
  resolve: {
    dedupe: ["react", "react-dom", "@tanstack/react-query"],
    alias: [
      { find: /^react$/, replacement: resolve(desktop, "react") },
      { find: /^react\/(.*)$/, replacement: resolve(desktop, "react") + "/$1" },
      { find: /^react-dom$/, replacement: resolve(desktop, "react-dom") },
      { find: /^react-dom\/(.*)$/, replacement: resolve(desktop, "react-dom") + "/$1" },
      // Only the passkey ceremony, the role and the workspace are stubbed; the card, viewer and approval flow are the product code.
      { find: /^@multica\/core\/auth\/webauthn$/, replacement: resolve(__dirname, "shims/webauthn.ts") },
      { find: /^@multica\/core\/permissions$/, replacement: resolve(__dirname, "shims/permissions.ts") },
      { find: /^@multica\/core\/paths$/, replacement: resolve(__dirname, "shims/paths.ts") },
    ],
  },
};
