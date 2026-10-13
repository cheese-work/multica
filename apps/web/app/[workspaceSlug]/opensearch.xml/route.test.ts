import { afterEach, describe, expect, it, vi } from "vitest";
import { GET } from "./route";

afterEach(() => vi.unstubAllEnvs());

async function getDocument(slug = "acme", origin = "https://app.multica.test") {
  return GET(new Request(`${origin}/${encodeURIComponent(slug)}/opensearch.xml`), {
    params: Promise.resolve({ workspaceSlug: slug }),
  });
}

describe("workspace OpenSearch description", () => {
  it("advertises the temporary suggestions probe only when explicitly enabled", async () => {
    vi.stubEnv("MULTICA_COOKIE_PROBE", "1");
    const response = await getDocument();
    const document = new DOMParser().parseFromString(await response.text(), "application/xml");
    const suggestions = document.querySelector('Url[type="application/x-suggestions+json"]');
    expect(suggestions?.getAttribute("template")).toBe(
      "https://app.multica.test/acme/suggestions-probe?q={searchTerms}",
    );
    expect(document.querySelectorAll("Url")).toHaveLength(2);
  });

  it.each([undefined, "0", "true"])("keeps the probe disabled for %j", async (enabled) => {
    vi.stubEnv("MULTICA_COOKIE_PROBE", enabled);
    expect(await (await getDocument()).text()).not.toContain("application/x-suggestions+json");
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
    expect(urls).toHaveLength(1);
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
      "ShortName", "Description", "InputEncoding", "Url",
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
