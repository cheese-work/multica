// @vitest-environment node
import { afterEach, describe, expect, it } from "vitest";
import { configStore } from "@multica/core/config";
import { docsUrl } from "./docs-url";

afterEach(() => {
  configStore.getState().setDocsBaseUrl(undefined);
  configStore.getState().setDaemonConfig({});
});

describe("docsUrl", () => {
  it("falls back to multica.ai when the instance serves no docs", () => {
    expect(docsUrl()).toBe("https://multica.ai/docs");
    expect(docsUrl("/agents", "fr")).toBe("https://multica.ai/docs/fr/agents");
  });

  it("resolves an advertised path against the app URL when there is no page origin", () => {
    configStore.getState().setDaemonConfig({ daemonAppUrl: "https://host.example" });
    configStore.getState().setDocsBaseUrl("/docs");
    expect(docsUrl()).toBe("https://host.example/docs");
    expect(docsUrl("/github-integration")).toBe(
      "https://host.example/docs/github-integration",
    );
    expect(docsUrl("/agents", "zh-Hans")).toBe("https://host.example/docs/zh/agents");
  });

  it("falls back to upstream when a path is advertised but no origin is known", () => {
    configStore.getState().setDocsBaseUrl("/docs");
    expect(docsUrl("/agents")).toBe("https://multica.ai/docs/agents");
  });
});
