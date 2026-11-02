// @vitest-environment node
import { afterEach, describe, expect, it } from "vitest";
import { configStore } from "@multica/core/config";
import { docsUrl } from "./docs-url";

afterEach(() => configStore.getState().setDocsBaseUrl(undefined));

describe("docsUrl", () => {
  it("falls back to multica.ai when the instance serves no docs", () => {
    expect(docsUrl()).toBe("https://multica.ai/docs");
    expect(docsUrl("/agents", "fr")).toBe("https://multica.ai/docs/fr/agents");
  });

  it("uses the instance docs when the server advertises them", () => {
    configStore.getState().setDocsBaseUrl("https://host.example/docs/");
    expect(docsUrl()).toBe("https://host.example/docs");
    expect(docsUrl("/github-integration")).toBe(
      "https://host.example/docs/github-integration",
    );
    expect(docsUrl("/agents", "zh-Hans")).toBe("https://host.example/docs/zh/agents");
  });
});
