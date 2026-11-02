import { configStore } from "@multica/core/config";
import { docsLocalePrefix } from "./docs-locale";

const UPSTREAM_DOCS = "https://multica.ai/docs";

// Docs link for `path` ("/agents"). Uses the instance's own docs when the
// server advertises them (`docs_url`), else the upstream site. Reads the store
// at call time: config loads before the app shell renders any docs link.
export function docsUrl(path = "", language?: string): string {
  const base = configStore.getState().docsBaseUrl.replace(/\/+$/, "") || UPSTREAM_DOCS;
  return `${base}${docsLocalePrefix(language)}${path}`;
}
