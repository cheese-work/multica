// Fidelity harness build (CHE-5793 step 2): the pattern harness config with its own small entry, so
// check-fidelity renders the real component without the whole pattern catalogue.
import { defineConfig, mergeConfig } from "vite";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";
import base from "./vite.config.mjs";

const here = fileURLToPath(new URL(".", import.meta.url));
export default mergeConfig(
  base,
  defineConfig({ build: { rolldownOptions: { input: resolve(here, "fidelity.html") } } }),
);
