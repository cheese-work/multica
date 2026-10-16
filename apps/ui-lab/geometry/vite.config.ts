import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
export default defineConfig({
  root: __dirname,
  // Serves the harness only; the repo is read by relative path.
  plugins: [react(), tailwindcss()],
  server: { fs: { strict: false } },
});
