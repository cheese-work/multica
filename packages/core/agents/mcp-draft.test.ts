import { describe, expect, it } from "vitest";
import {
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
    expect(JSON.parse(redactMcpJsonText('{"name":"x","options":[{"token":"s"}]}'))).toEqual({
      name: "x",
      options: [{}],
    });
  });

  it.each([
    ['{"url": "https://a.test", "headers": {"Authorization": "Bearer s3cret"', "https://a.test"],
    ['{"command": "npx", "env": {"API_KEY": "s3cret"}, "args": ["-y"', "npx"],
    ['{"url": "https://a.test",\n  "token": "s3cr', "https://a.test"],
    ['{"options": [{"api-token":\n"s3cret"}], "url": "https', "options"],
    ['{"url": "https://a.test", "token": Bearer s3cret', "https://a.test"],
    ['{"url": "https://a.test", "token": Bearer "s3cret" tail', "https://a.test"],
    ['{"url": "https://a.test", "token": Bearer\ts3cret }', "https://a.test"],
    ['{"url": "https://a.test", "token": Bearer\ns3cret', "https://a.test"],
    ['{"url": "https://a.test", "private_key": -----BEGIN KEY-----\ns3cretbody\nmore3cret\n-----END KEY-----', "https://a.test"],
    ['{"url": "https://a.test", "token": "s3cret" "x": "y"', "https://a.test"],
    ['{"url": "https://a.test", "t\\u006fken": s3cret', "https://a.test"],
    ['{"url": "https://a.test", token: s3cret', "https://a.test"],
    ["{'url': 'https://a.test', 'token': s3cret", "https://a.test"],
  ])("keeps unfinished text but never the credential value: %s", (text, kept) => {
    const out = redactMcpJsonText(text);
    expect(out).not.toContain("s3cret");
    expect(out).not.toContain("s3cr");
    expect(out).toContain(kept);
  });

  it.each([
    '{"headers":{"Authorization":"s3cret"},"url":"https://a.test","note":"unsaved",',
    '{"url":"https://a.test","token": Bearer s3cret, "note":"unsaved",',
    '{"url":"https://a.test","private_key": -----BEGIN KEY-----\ns3cretbody\n-----END KEY-----,\n"note":"unsaved",',
    '{"url":"https://a.test","token": Bearer\ts3cret,\n"note":"unsaved",',
    '{"url":"https://a.test","mcp": {"api_key": s3cret\nx}, "note":"unsaved",',
    '{"url":"https://a.test","options": [{"token": "s3cret", "name": "n"}], "note":"unsaved",',
  ])("keeps the non-secret fields on both sides of a credential: %s", (text) => {
    const out = redactMcpJsonText(text);
    expect(out).not.toContain("s3cret");
    expect(out).toContain('"url":"https://a.test"');
    expect(out).toContain('"note":"unsaved"');
  });

  it("does not treat a value that mentions a credential word as a key", () => {
    expect(redactMcpJsonText('{"url": "https://a.test/token", "x"')).toContain("https://a.test/token");
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
