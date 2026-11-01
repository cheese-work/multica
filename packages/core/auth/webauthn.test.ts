// @vitest-environment node

import { afterEach, describe, expect, it, vi } from "vitest";
import { base64urlToBytes, bytesToBase64url, createPasskey, getPasskeyAssertion, signPasskeyChallenge } from "./webauthn";

afterEach(() => vi.unstubAllGlobals());

const bytes = (...values: number[]) => new Uint8Array(values).buffer;

describe("base64url", () => {
  it("round-trips bytes with no padding and the URL alphabet", () => {
    const raw = new Uint8Array([251, 255, 254, 0, 1]);
    const text = bytesToBase64url(raw);
    expect(text).toBe("-__-AAE");
    expect(Array.from(base64urlToBytes(text))).toEqual(Array.from(raw));
  });

  it("rejects text outside the alphabet", () => {
    expect(() => base64urlToBytes("a+b/")).toThrow();
  });
});

describe("ceremonies", () => {
  it("decodes creation options and encodes the attestation", async () => {
    const create = vi.fn().mockResolvedValue({
      id: "cred", rawId: bytes(1, 2), type: "public-key", authenticatorAttachment: "platform",
      response: { attestationObject: bytes(3), clientDataJSON: bytes(4), getTransports: () => ["internal"] },
      getClientExtensionResults: () => ({}),
    });
    vi.stubGlobal("navigator", { credentials: { create } });
    const result = await createPasskey({
      publicKey: { challenge: "AQI", user: { id: "Aw", name: "u", displayName: "u" }, excludeCredentials: [{ id: "BA", type: "public-key" }] },
    });
    const sent = create.mock.calls[0]?.[0].publicKey;
    expect(Array.from(new Uint8Array(sent.challenge))).toEqual([1, 2]);
    expect(Array.from(new Uint8Array(sent.user.id))).toEqual([3]);
    expect(Array.from(new Uint8Array(sent.excludeCredentials[0].id))).toEqual([4]);
    expect(result).toEqual({
      id: "cred", rawId: "AQI", type: "public-key", authenticatorAttachment: "platform",
      response: { attestationObject: "Aw", clientDataJSON: "BA", transports: ["internal"] },
      clientExtensionResults: {},
    });
  });

  it("decodes request options and encodes the assertion, omitting a null user handle", async () => {
    const get = vi.fn().mockResolvedValue({
      id: "cred", rawId: bytes(1), type: "public-key",
      response: { authenticatorData: bytes(2), clientDataJSON: bytes(3), signature: bytes(4), userHandle: null },
      getClientExtensionResults: () => ({}),
    });
    vi.stubGlobal("navigator", { credentials: { get } });
    const result = await getPasskeyAssertion({ publicKey: { challenge: "AQ", allowCredentials: [{ id: "Ag", type: "public-key" }] } });
    expect(Array.from(new Uint8Array(get.mock.calls[0]?.[0].publicKey.allowCredentials[0].id))).toEqual([2]);
    expect(result).toEqual({
      id: "cred", rawId: "AQ", type: "public-key",
      response: { authenticatorData: "Ag", clientDataJSON: "Aw", signature: "BA" },
      clientExtensionResults: {},
    });
  });

  it("fails when the browser has no WebAuthn or the user cancels", async () => {
    vi.stubGlobal("navigator", {});
    await expect(getPasskeyAssertion({ publicKey: { challenge: "AQ" } })).rejects.toThrow(/not supported/);
    vi.stubGlobal("navigator", { credentials: { get: vi.fn().mockResolvedValue(null) } });
    await expect(getPasskeyAssertion({ publicKey: { challenge: "AQ" } })).rejects.toThrow(/cancel/);
  });
});

describe("signPasskeyChallenge", () => {
  it("wraps the credential with the challenge id, the envelope the server verifies", async () => {
    const get = vi.fn().mockResolvedValue({
      id: "cred", rawId: bytes(1), type: "public-key",
      response: { authenticatorData: bytes(2), clientDataJSON: bytes(3), signature: bytes(4), userHandle: null },
      getClientExtensionResults: () => ({}),
    });
    vi.stubGlobal("navigator", { credentials: { get } });
    const signed = await signPasskeyChallenge({ challenge_id: "ch1", options: { publicKey: { challenge: "AQ" } }, expires_at: "t" });
    expect(Object.keys(signed).sort()).toEqual(["challenge_id", "credential"]);
    expect(signed.challenge_id).toBe("ch1");
    expect(signed.credential).toMatchObject({ id: "cred", response: { signature: "BA" } });
  });
});
