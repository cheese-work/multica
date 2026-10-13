import { createServer } from "node:http";
import { GET as description } from "../apps/web/app/[workspaceSlug]/opensearch.xml/route";
import { GET as suggestions } from "../apps/web/app/[workspaceSlug]/suggestions-probe/route";

if (process.env.MULTICA_COOKIE_PROBE !== "1") {
  throw new Error("Set MULTICA_COOKIE_PROBE=1 to run the temporary probe");
}
const origin = new URL(process.env.COOKIE_PROBE_ORIGIN ?? "http://127.0.0.1:19693");
if (!["http:", "https:"].includes(origin.protocol) || origin.username || origin.password || origin.pathname !== "/" || origin.search || origin.hash) {
  throw new Error("COOKIE_PROBE_ORIGIN must be an HTTP(S) origin");
}
const port = Number(process.env.COOKIE_PROBE_PORT ?? "19693");
const workspaceSlug = "cheese-work";
const context = { params: Promise.resolve({ workspaceSlug }) };
const counts = { withCookie: 0, withoutCookie: 0 };
const page = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Multica cookie probe</title>
<link rel="search" type="application/opensearchdescription+xml" title="Multica cookie probe" href="/${workspaceSlug}/opensearch.xml">
</head><body><h1>Multica cookie probe</h1>
<p>Sign in to Multica on this same hostname in a separate tab first. Do not copy or paste any cookie.</p>
<ol><li>Note the counts below.</li><li>Activate this origin under Inactive shortcuts in Brave's Search engine settings. Set its shortcut to mcprobe.</li>
<li>Type mcprobe, press Tab, type cookieprobe without pressing Enter, and return to this tab.</li></ol>
<p>Report the before/after counts, Brave version from brave://version, and the date on CHE-1593.</p>
<p>The probe records only session-cookie presence. No cookie values, queries, account data or workspace data are recorded.</p>
<pre id="counts" role="status">Loading counts</pre>
<script>async function refresh() { try { const response = await fetch('/probe-status', {cache: 'no-store'}); const counts = await response.json(); document.getElementById('counts').textContent = JSON.stringify(counts, null, 2); } catch { document.getElementById('counts').textContent = 'Probe unavailable'; } } refresh(); setInterval(refresh, 1000);</script>
</body></html>`;

const server = createServer(async (incoming, outgoing) => {
  try {
    if (incoming.method !== "GET" && incoming.method !== "HEAD") {
      outgoing.writeHead(405, { Allow: "GET, HEAD" });
      outgoing.end();
      return;
    }
    const url = new URL(incoming.url ?? "/", origin);
    let response: Response;
    if (url.pathname === "/" || url.pathname === `/${workspaceSlug}/issues`) {
      response = new Response(page, { headers: { "Content-Type": "text/html; charset=utf-8" } });
    } else if (url.pathname === "/probe-status") {
      response = Response.json(counts);
    } else if (url.pathname === `/${workspaceSlug}/opensearch.xml`) {
      response = await description(new Request(url, {
        headers: { host: origin.host, "x-forwarded-proto": origin.protocol.slice(0, -1) },
      }), context);
    } else if (url.pathname === `/${workspaceSlug}/suggestions-probe` && incoming.method === "GET") {
      response = await suggestions(new Request(url, {
        headers: { cookie: incoming.headers.cookie ?? "" },
      }), context);
      if (response.headers.get("x-multica-cookie-probe") === "present") counts.withCookie++;
      else counts.withoutCookie++;
    } else {
      response = new Response("Not found", { status: 404 });
    }
    response.headers.set("Cache-Control", "no-store");
    response.headers.set("X-Content-Type-Options", "nosniff");
    outgoing.writeHead(response.status, Object.fromEntries(response.headers));
    outgoing.end(incoming.method === "HEAD" ? undefined : await response.text());
  } catch {
    outgoing.writeHead(500, { "Cache-Control": "no-store" });
    outgoing.end("Probe unavailable");
  }
});

server.listen(port, "127.0.0.1", () => console.info(`Cookie probe listening on loopback port ${port}`));
process.on("SIGTERM", () => server.close());
process.on("SIGINT", () => server.close());
