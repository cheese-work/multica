import { runtimeRewriteDestination } from "@/config/runtime-urls";

const hopByHopHeaders = [
  "connection",
  "keep-alive",
  "proxy-authenticate",
  "proxy-authorization",
  "te",
  "trailer",
  "transfer-encoding",
  "upgrade",
];

type SearchApiPath = "/api/search/suggestions" | "/api/issues/search" | "/api/projects/search";

function errorCode(error: unknown): string {
  const cause = error instanceof Error ? error.cause : undefined;
  if (typeof cause === "object" && cause !== null && "code" in cause && typeof cause.code === "string") {
    return cause.code;
  }
  return error instanceof Error ? error.name : "unknown";
}

export async function proxySearchApi(request: Request, pathname: SearchApiPath): Promise<Response> {
  const destination = runtimeRewriteDestination(pathname, process.env);
  if (!destination) return new Response(null, { status: 404 });

  const upstreamUrl = new URL(destination);
  upstreamUrl.search = new URL(request.url).search;
  const requestHeaders = new Headers(request.headers);
  requestHeaders.delete("host");
  for (const header of hopByHopHeaders) requestHeaders.delete(header);

  try {
    const upstream = await fetch(upstreamUrl, {
      cache: "no-store",
      headers: requestHeaders,
      redirect: "manual",
      signal: request.signal,
    });
    const responseHeaders = new Headers(upstream.headers);
    for (const header of hopByHopHeaders) responseHeaders.delete(header);
    return new Response(upstream.body, {
      status: upstream.status,
      statusText: upstream.statusText,
      headers: responseHeaders,
    });
  } catch (error) {
    if (request.signal.aborted) {
      return new Response(null, { status: 499, headers: { "cache-control": "no-store" } });
    }
    console.error("Search proxy failed", pathname, errorCode(error));
    return new Response(null, { status: 502, headers: { "cache-control": "no-store" } });
  }
}
