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

function errorCode(error: unknown): string {
  const cause = error instanceof Error ? error.cause : undefined;
  if (typeof cause === "object" && cause !== null && "code" in cause && typeof cause.code === "string") {
    return cause.code;
  }
  return error instanceof Error ? error.name : "unknown";
}

export async function GET(request: Request): Promise<Response> {
  const destination = runtimeRewriteDestination("/api/search/suggestions", process.env);
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
    });
    const responseHeaders = new Headers(upstream.headers);
    for (const header of hopByHopHeaders) responseHeaders.delete(header);
    return new Response(upstream.body, {
      status: upstream.status,
      statusText: upstream.statusText,
      headers: responseHeaders,
    });
  } catch (error) {
    console.error("Search suggestions proxy failed", errorCode(error));
    return new Response(null, { status: 502, headers: { "cache-control": "no-store" } });
  }
}
