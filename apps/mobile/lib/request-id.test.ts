import { afterEach, describe, expect, it, vi } from "vitest";
import { createIdempotencyKey } from "./request-id";

const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

afterEach(() => vi.unstubAllGlobals());

describe("createIdempotencyKey", () => {
  it("is an RFC 4122 v4 UUID when the runtime has no crypto (server rejects non-UUID request_id)", () => {
    vi.stubGlobal("crypto", undefined);
    expect(createIdempotencyKey()).toMatch(UUID_V4);
  });

  it("is an RFC 4122 v4 UUID when crypto exists without randomUUID", () => {
    vi.stubGlobal("crypto", {});
    expect(createIdempotencyKey()).toMatch(UUID_V4);
  });

  it("fallback keys do not repeat", () => {
    vi.stubGlobal("crypto", undefined);
    const keys = new Set(Array.from({ length: 500 }, () => createIdempotencyKey()));
    expect(keys.size).toBe(500);
  });
});
