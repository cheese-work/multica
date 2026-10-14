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
    const publicUrl = new URL(process.env.MULTICA_APP_URL?.trim() || `${protocol}://${host}`);
    if (!/^https?:$/.test(publicUrl.protocol) || publicUrl.username || publicUrl.password
      || publicUrl.pathname !== "/" || publicUrl.search || publicUrl.hash) {
      return new Response("Invalid origin", { status: 400 });
    }
    origin = publicUrl.origin;
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
