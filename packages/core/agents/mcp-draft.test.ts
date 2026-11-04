import { describe, expect, it } from "vitest";
import { restoreMcpSecrets, stripMcpSecrets } from "./mcp-draft";

describe("stripMcpSecrets", () => {
  it("drops credential-named keys at any depth and keeps the rest", () => {
    expect(
      stripMcpSecrets({
        type: "http",
        url: "https://a.test",
        headers: { Authorization: "Bearer x" },
        mcp: { command: "npx", env: { API_KEY: "k" }, args: ["-y"] },
      }),
    ).toEqual({ type: "http", url: "https://a.test", mcp: { command: "npx", args: ["-y"] } });
  });
});

describe("restoreMcpSecrets", () => {
  it("takes credential keys from the saved config, never from the draft", () => {
    expect(
      restoreMcpSecrets(
        { url: "https://a.test", headers: { Authorization: "saved" }, mcp: { env: { K: "saved" } } },
        { url: "https://b.test", mcp: { command: "npx" } },
      ),
    ).toEqual({
      url: "https://b.test",
      headers: { Authorization: "saved" },
      mcp: { env: { K: "saved" }, command: "npx" },
    });
  });

  it("restores nothing secret when there is no saved config", () => {
    expect(restoreMcpSecrets({}, { url: "https://b.test" })).toEqual({ url: "https://b.test" });
  });
});
