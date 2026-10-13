export async function GET(
  request: Request,
  { params }: { params: Promise<{ workspaceSlug: string }> },
) {
  const { workspaceSlug } = await params;
  if (process.env.MULTICA_COOKIE_PROBE !== "1" || !/^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(workspaceSlug)) {
    return new Response("Not found", { status: 404 });
  }

  const sessionCookiePresent = /(?:^|;)\s*multica_auth=/.test(request.headers.get("cookie") ?? "");
  console.info("Multica cookie probe: session cookie present", sessionCookiePresent);
  return new Response(JSON.stringify([
    new URL(request.url).searchParams.get("q") ?? "",
    ["Multica cookie probe"],
  ]), {
    headers: {
      "Content-Type": "application/x-suggestions+json; charset=utf-8",
      "Cache-Control": "no-store",
      "X-Multica-Cookie-Probe": sessionCookiePresent ? "present" : "absent",
    },
  });
}
