import { configStore } from "@multica/core/config";
import { instanceOrigin } from "@multica/core/config/instance-origin";
import { docsLocalePrefix } from "./docs-locale";

const UPSTREAM_DOCS = "https://multica.ai/docs";

// The server advertises a path ("/docs"), not an origin: the same instance can
// be reached by several hostnames. Web pages resolve it against the origin the
// visitor is on; the desktop shell (non-http origin) uses the app URL.
function instanceDocsBase(): string | undefined {
  const { docsBaseUrl, daemonAppUrl } = configStore.getState();
  if (!docsBaseUrl) return undefined;
  if (!docsBaseUrl.startsWith("/")) return docsBaseUrl;
  const origin = instanceOrigin(daemonAppUrl);
  return origin ? origin + docsBaseUrl : undefined;
}

// Docs link for `path` ("/agents"). Uses the instance's own docs when the
// server advertises them (`docs_url`), else the upstream site. Reads the store
// at call time: config loads before the app shell renders any docs link.
export function docsUrl(path = "", language?: string): string {
  const base = (instanceDocsBase() ?? UPSTREAM_DOCS).replace(/\/+$/, "");
  return `${base}${docsLocalePrefix(language)}${path}`;
}
