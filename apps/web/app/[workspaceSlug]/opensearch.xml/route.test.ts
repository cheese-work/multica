import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { GET } from "./route";
async function getDocument(slug = "acme", origin = "https://app.multica.test") {
  return GET(new Request(`${origin}/${encodeURIComponent(slug)}/opensearch.xml`), {
    params: Promise.resolve({ workspaceSlug: slug }),
  });
}

describe("workspace OpenSearch description", () => {
  beforeEach(() => {
    vi.stubEnv("MULTICA_APP_URL", "");
    vi.stubEnv("MULTICA_COOKIE_PROBE", "");
  });
  afterEach(() => vi.unstubAllEnvs());
  it("advertises the stable authenticated suggestions endpoint", async () => {
    const response = await getDocument();
    const document = new DOMParser().parseFromString(await response.text(), "application/xml");
    const suggestions = document.querySelector('Url[type="application/x-suggestions+json"]');
    expect(suggestions?.getAttribute("template")).toBe(
      "https://app.multica.test/api/search/suggestions?workspace_slug=acme&q={searchTerms}",
    );
    expect(document.querySelectorAll("Url")).toHaveLength(2);
  });

  it("serves a public UTF-8 XML description without authentication", async () => {
    const response = await getDocument();
    expect(response.status).toBe(200);
    expect(response.headers.get("content-type")).toBe(
      "application/opensearchdescription+xml; charset=utf-8",
    );
    expect(response.headers.get("cache-control")).toBe("no-store");
    expect(response.headers.get("set-cookie")).toBeNull();
    const document = new DOMParser().parseFromString(await response.text(), "application/xml");
    expect(document.querySelector("parsererror")).toBeNull();
    expect(document.documentElement.localName).toBe("OpenSearchDescription");
    expect(document.documentElement.namespaceURI).toBe("http://a9.com/-/spec/opensearch/1.1/");
    expect(document.querySelector("ShortName")?.textContent).toBe("Multica acme");
    expect(document.querySelector("InputEncoding")?.textContent).toBe("UTF-8");
    const urls = document.querySelectorAll("Url");
    expect(urls).toHaveLength(2);
    expect(urls[0]?.getAttribute("type")).toBe("text/html");
    expect(urls[0]?.getAttribute("template")).toBe(
      "https://app.multica.test/acme/search?q={searchTerms}",
    );
  });

  it("uses the requesting app origin including self-hosted ports", async () => {
    const response = await getDocument("my-team", "http://localhost:3100");
    expect(await response.text()).toContain(
      'template="http://localhost:3100/my-team/search?q={searchTerms}"',
    );
  });

  it("uses the public reverse-proxy origin instead of the Next.js bind address", async () => {
    const response = await GET(new Request("http://0.0.0.0:3000/acme/opensearch.xml", {
      headers: { host: "app.example.com:8443", "x-forwarded-proto": "https" },
    }), { params: Promise.resolve({ workspaceSlug: "acme" }) });
    expect(await response.text()).toContain(
      'template="https://app.example.com:8443/acme/search?q={searchTerms}"',
    );
  });

  it("uses the configured C00 public origin despite nginx's internal HTTP headers", async () => {
    vi.stubEnv("MULTICA_APP_URL", "https://congvc-c00.taila6fa8a.ts.net:19443");
    const response = await GET(new Request("http://0.0.0.0:3000/cheese-work/opensearch.xml", {
      headers: { host: "congvc-c00.taila6fa8a.ts.net", "x-forwarded-proto": "http" },
    }), { params: Promise.resolve({ workspaceSlug: "cheese-work" }) });
    expect(response.status).toBe(200);
    expect(await response.text()).toContain(
      'template="https://congvc-c00.taila6fa8a.ts.net:19443/cheese-work/search?q={searchTerms}"',
    );
  });

  it("falls back to nginx's internal HTTP origin on C00 when no public origin is configured", async () => {
    const response = await GET(new Request("http://0.0.0.0:3000/cheese-work/opensearch.xml", {
      headers: { host: "congvc-c00.taila6fa8a.ts.net", "x-forwarded-proto": "http" },
    }), { params: Promise.resolve({ workspaceSlug: "cheese-work" }) });
    expect(response.status).toBe(200);
    expect(await response.text()).toContain(
      'template="http://congvc-c00.taila6fa8a.ts.net/cheese-work/search?q={searchTerms}"',
    );
  });

  it("does not let request headers override the configured public origin", async () => {
    vi.stubEnv("MULTICA_APP_URL", "https://app.example.com:8443/");
    const response = await GET(new Request("http://0.0.0.0:3000/acme/opensearch.xml", {
      headers: { host: "attacker.test:1234", "x-forwarded-proto": "http" },
    }), { params: Promise.resolve({ workspaceSlug: "acme" }) });
    expect(await response.text()).toContain(
      'template="https://app.example.com:8443/acme/search?q={searchTerms}"',
    );
  });

  it("reflects an unconfigured request host only in its own uncached response", async () => {
    const response = await GET(new Request("http://0.0.0.0:3000/acme/opensearch.xml", {
      headers: { host: "attacker.test:8443", "x-forwarded-proto": "https" },
    }), { params: Promise.resolve({ workspaceSlug: "acme" }) });
    expect(await response.text()).toContain(
      'template="https://attacker.test:8443/acme/search?q={searchTerms}"',
    );
    expect(response.headers.get("cache-control")).toBe("no-store");
    expect(response.headers.get("set-cookie")).toBeNull();
    const nextResponse = await getDocument();
    expect(await nextResponse.text()).toContain(
      'template="https://app.multica.test/acme/search?q={searchTerms}"',
    );
  });

  it.each([
    "ftp://app.example.com", "https://user:pass@app.example.com",
    "https://app.example.com/path", "https://app.example.com?override=1",
    "https://app.example.com#fragment", "/relative", "http://",
  ])("rejects an invalid configured public origin %j", async (origin) => {
    vi.stubEnv("MULTICA_APP_URL", origin);
    expect((await getDocument()).status).toBe(400);
  });

  it.each(["ftp", "https,http"])("rejects an invalid forwarded protocol %j", async (protocol) => {
    const response = await GET(new Request("http://0.0.0.0:3000/acme/opensearch.xml", {
      headers: { host: "app.example.com", "x-forwarded-proto": protocol },
    }), { params: Promise.resolve({ workspaceSlug: "acme" }) });
    expect(response.status).toBe(400);
  });

  it.each(["attacker.test/path", "attacker.test@other.test", "one.test,two.test"])(
    "rejects an invalid origin host %j",
    async (host) => {
      const response = await GET(new Request("http://0.0.0.0:3000/acme/opensearch.xml", {
        headers: { host },
      }), { params: Promise.resolve({ workspaceSlug: "acme" }) });
      expect(response.status).toBe(400);
    },
  );

  it("escapes XML characters in the origin", async () => {
    const response = await getDocument("acme", "https://app&example.test");
    const document = new DOMParser().parseFromString(await response.text(), "application/xml");
    expect(document.querySelector("parsererror")).toBeNull();
    expect(document.querySelector("Url")?.getAttribute("template")).toBe(
      "https://app&example.test/acme/search?q={searchTerms}",
    );
  });

  it("does not reveal workspace existence or private metadata", async () => {
    const response = await getDocument("nonexistent-workspace");
    expect(response.status).toBe(200);
    const document = new DOMParser().parseFromString(await response.text(), "application/xml");
    expect(Array.from(document.documentElement.children, (element) => element.localName)).toEqual([
      "ShortName", "Description", "InputEncoding", "Url", "Url",
    ]);
    expect(document.querySelector("ShortName")?.textContent?.length).toBeLessThanOrEqual(16);
    expect(document.querySelector("Url")?.getAttribute("template")).toContain(
      "/nonexistent-workspace/search?q={searchTerms}",
    );
  });

  it.each(["", "ACME", "acme/team", 'acme"><Url', "acme&team", "-acme", "acme--team"])(
    "rejects the invalid slug %j without returning XML",
    async (slug) => {
      const response = await getDocument(slug);
      expect(response.status).toBe(404);
      expect(response.headers.get("content-type")).not.toContain("opensearchdescription");
    },
  );
});
