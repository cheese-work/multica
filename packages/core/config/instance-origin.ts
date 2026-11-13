// One instance can be reached through several origins (Cloudflare, Tailscale,
// ...), while the server advertises a single canonical app URL
// (`daemon_app_url`). Web pages link to the origin the visitor is on; the
// desktop shell (non-http origin) has none, so it uses the app URL.
// Returns "" when neither is known.
export function instanceOrigin(daemonAppUrl?: string | null): string {
  if (typeof location !== "undefined" && /^https?:$/.test(location.protocol)) {
    return location.origin;
  }
  return daemonAppUrl?.trim().replace(/\/+$/, "") ?? "";
}
