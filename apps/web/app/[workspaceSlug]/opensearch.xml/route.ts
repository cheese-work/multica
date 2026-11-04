export async function GET(
  request: Request,
  { params }: { params: Promise<{ workspaceSlug: string }> },
) {
  const { workspaceSlug } = await params;
  if (!/^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(workspaceSlug)) {
    return new Response("Not found", { status: 404 });
  }

  const requestUrl = new URL(request.url);
  const host = request.headers.get("host") ?? requestUrl.host;
  const protocol = request.headers.get("x-forwarded-proto") ?? requestUrl.protocol.slice(0, -1);
  if (!/^(http|https)$/.test(protocol) || /[,/\\?#@\s]/.test(host)) {
    return new Response("Invalid origin", { status: 400 });
  }
  let origin: string;
  try {
    const configured = process.env.MULTICA_APP_URL?.trim();
    const publicUrl = new URL(configured || `${protocol}://${host}`);
    // An https configured origin means the instance is served over TLS; the
    // proxy hop's X-Forwarded-Proto: http must not downgrade alternate hosts.
    const visitor = new URL(`${publicUrl.protocol === "https:" ? "https" : protocol}://${host}`);
    if (!/^https?:$/.test(publicUrl.protocol) || publicUrl.username || publicUrl.password
      || publicUrl.pathname !== "/" || publicUrl.search || publicUrl.hash) {
      return new Response("Invalid origin", { status: 400 });
    }
    // The configured URL is one canonical origin, but the instance can be
    // reached through others (Cloudflare, Tailscale). It only wins when the
    // visitor is on the same hostname, where it restores the port and scheme
    // an internal proxy hop drops; any other visitor host is used as is. The
    // response is no-store and per request, so a spoofed Host only affects
    // the caller.
    origin = publicUrl.hostname === visitor.hostname ? publicUrl.origin : visitor.origin;
  } catch {
    return new Response("Invalid origin", { status: 400 });
  }
  const template = `${origin}/${workspaceSlug}/search?q={searchTerms}`
    .replaceAll("&", "&amp;")
    .replaceAll('"', "&quot;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;");
  const suggestions = `${origin}/api/search/suggestions?workspace_slug=${workspaceSlug}&q={searchTerms}`
    .replaceAll("&", "&amp;")
    .replaceAll('"', "&quot;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;");

  return new Response(
    `<?xml version="1.0" encoding="UTF-8"?>
<OpenSearchDescription xmlns="http://a9.com/-/spec/opensearch/1.1/">
  <ShortName>${`Multica ${workspaceSlug}`.slice(0, 16)}</ShortName>
  <Description>Search issues, projects and agents in Multica.</Description>
  <InputEncoding>UTF-8</InputEncoding>
  <Url type="text/html" template="${template}" />
  <Url type="application/x-suggestions+json" template="${suggestions}" />
</OpenSearchDescription>`,
    {
      headers: {
        "Content-Type": "application/opensearchdescription+xml; charset=utf-8",
        "Cache-Control": "no-store",
      },
    },
  );
}
