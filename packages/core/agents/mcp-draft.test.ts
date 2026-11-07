import { describe, expect, it } from "vitest";
import {
  credentialsDropped,
  destinationMoved,
  fillRedactedSecrets,
  redactMcpJsonText,
  restoreMcpSecrets,
  stripMcpSecrets,
} from "./mcp-draft";

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

describe("stripMcpSecrets in arrays", () => {
  it("drops credential keys inside array elements", () => {
    expect(
      stripMcpSecrets({ url: "https://example.test", options: [{ token: "secret", name: "a" }, [{ password: "p" }]] }),
    ).toEqual({ url: "https://example.test", options: [{ name: "a" }, [{}]] });
  });
});

describe("redactMcpJsonText", () => {
  it("strips a parsable object without needing a url or command", () => {
    expect(JSON.parse(redactMcpJsonText('{"name":"x","options":[{"token":"s"}]}')!)).toEqual({
      name: "x",
      options: [{}],
    });
  });

  it("strips credential keys from a finished object by key name", () => {
    expect(
      JSON.parse(redactMcpJsonText('{"url":"https://a.test","headers":{"Authorization":"s3cret"},"note":"x"}')!),
    ).toEqual({ url: "https://a.test", note: "x" });
  });

  it.each([
    '{"url": "https://a.test", "headers": {"Authorization": "Bearer s3cret"',
    '{"password": abc, region: synthetic_secret',
    '{"url": "https://a.test", "token": Bearer s3cret, "note": "x"}',
    '{"url": "https://a.test", "private_key": -----BEGIN KEY-----\ns3cretbody\n-----END KEY-----',
    '{"url": "https://a.test", token: s3cret',
    '[1, 2',
    '"s3cret"',
    "",
  ])("returns null instead of guessing where a credential ends: %s", (text) => {
    expect(redactMcpJsonText(text)).toBeNull();
  });
});

describe("fillRedactedSecrets", () => {
  it("swaps null placeholders for saved values and leaves typed ones", () => {
    expect(
      fillRedactedSecrets(
        { headers: { A: "saved" }, mcp: { env: { K: "saved" } } },
        { headers: null, mcp: { env: null, token: "typed" }, other: null },
      ),
    ).toEqual({ headers: { A: "saved" }, mcp: { env: { K: "saved" }, token: "typed" }, other: null });
  });

  it("restores placeholders inside arrays by position", () => {
    expect(
      fillRedactedSecrets(
        { options: [{ token: "saved", name: "a" }, { name: "c" }] },
        { options: [{ token: null, name: "b" }, { name: "d", password: null }, { token: null }] },
      ),
    ).toEqual({ options: [{ token: "saved", name: "b" }, { name: "d" }, {}] });
  });

  it("drops a placeholder that has no saved value", () => {
    expect(fillRedactedSecrets({}, { url: "u", headers: null })).toEqual({ url: "u" });
  });
});

describe("credentialsDropped", () => {
  const saved = { type: "http", url: "https://a.test", headers: { A: "x", B: "y" } };

  it("is false while destination and credentials are untouched", () => {
    expect(credentialsDropped(saved, { ...saved, note: "n" })).toBe(false);
  });
  it("is true when the destination changed", () => {
    expect(credentialsDropped(saved, { ...saved, url: "https://b.test" })).toBe(true);
    expect(credentialsDropped({ command: "a", env: { K: "v" } }, { command: "b", env: { K: "v" } })).toBe(true);
  });
  it("is true when any saved credential is gone", () => {
    expect(credentialsDropped(saved, { type: "http", url: "https://a.test" })).toBe(true);
    expect(credentialsDropped(saved, { ...saved, headers: { A: "x" } })).toBe(true);
  });
  it("treats a null placeholder as still present", () => {
    expect(credentialsDropped(saved, { ...saved, headers: null })).toBe(false);
  });
  it("is false when nothing was saved", () => {
    expect(credentialsDropped({}, { url: "https://b.test" })).toBe(false);
  });
});

describe("destinationMoved", () => {
  it("compares url, command, args and type only", () => {
    const saved = { type: "http", url: "https://a.test", headers: { Authorization: "x" } };
    expect(destinationMoved(saved, { type: "http", url: "https://a.test", note: "n" })).toBe(false);
    expect(destinationMoved(saved, { type: "http", url: "https://b.test" })).toBe(true);
    expect(destinationMoved({ command: "uvx", args: ["a"] }, { command: "uvx", args: ["b"] })).toBe(true);
  });
  it("ignores a url inside a credential subtree", () => {
    expect(destinationMoved({ url: "u", oauth: { url: "x" } }, { url: "u", oauth: { url: "y" } })).toBe(false);
  });
});
