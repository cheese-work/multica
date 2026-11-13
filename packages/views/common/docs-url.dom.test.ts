import { afterEach, expect, it } from "vitest";
import { configStore } from "@multica/core/config";
import { docsUrl } from "./docs-url";

afterEach(() => configStore.getState().setDocsBaseUrl(undefined));

it("resolves an advertised path against the origin the visitor is on", () => {
  configStore.getState().setDocsBaseUrl("/docs");
  expect(docsUrl("/agents", "fr")).toBe(`${window.location.origin}/docs/fr/agents`);
});
