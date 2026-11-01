import { createHash } from "crypto";
import { describe, expect, it, vi } from "vitest";

vi.mock("electron", () => ({ app: { getPath: () => "/tmp/multica-test" } }));

import { downloadServerCli, serverCliKey } from "./cli-bootstrap";

const BIN = Buffer.from("#!/bin/sh\necho multica\n");
const SHA = createHash("sha256").update(BIN).digest("hex");

function fakeServer(opts: { sha?: string; keys?: string[]; status?: number } = {}) {
  const calls: { url: string; auth: string | null }[] = [];
  const keys = opts.keys ?? ["linux-amd64", "darwin-arm64"];
  const fetchImpl = vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    const headers = new Headers(init?.headers);
    calls.push({ url, auth: headers.get("authorization") });
    if (opts.status && opts.status !== 200) return new Response("no", { status: opts.status });
    const u = new URL(url);
    if (u.pathname === "/api/daemon/update") {
      return Response.json({
        commit: "0".repeat(40),
        version: "v1",
        binaries: Object.fromEntries(
          keys.map((k) => [k, { sha256: opts.sha ?? SHA, size: BIN.length }]),
        ),
      });
    }
    if (u.pathname === "/api/daemon/update/binary") return new Response(new Uint8Array(BIN));
    return new Response("nf", { status: 404 });
  });
  return { fetchImpl: fetchImpl as unknown as typeof fetch, calls };
}

describe("serverCliKey", () => {
  it("maps node platform/arch to the server's build keys", () => {
    expect(serverCliKey("linux", "x64")).toBe("linux-amd64");
    expect(serverCliKey("darwin", "arm64")).toBe("darwin-arm64");
  });

  it("rejects platforms the server does not build", () => {
    expect(() => serverCliKey("win32", "x64")).toThrow(/unsupported/);
    expect(() => serverCliKey("linux", "ia32")).toThrow(/unsupported/);
  });
});

describe("downloadServerCli", () => {
  const base = { serverUrl: "https://multica.example.com/", token: "mul_tok", platform: "linux" as const, arch: "x64" };

  it("downloads from the configured server, never from GitHub", async () => {
    const { fetchImpl, calls } = fakeServer();
    const bytes = await downloadServerCli({ ...base, fetchImpl });

    expect(bytes.equals(BIN)).toBe(true);
    expect(calls.map((c) => c.url)).toEqual([
      "https://multica.example.com/api/daemon/update",
      "https://multica.example.com/api/daemon/update/binary?os=linux&arch=amd64",
    ]);
    expect(calls.every((c) => c.auth === "Bearer mul_tok")).toBe(true);
    expect(calls.some((c) => /github/i.test(c.url))).toBe(false);
  });

  it("refuses a binary whose sha256 differs from the manifest", async () => {
    const { fetchImpl } = fakeServer({ sha: "f".repeat(64) });
    await expect(downloadServerCli({ ...base, fetchImpl })).rejects.toThrow(/sha256|checksum/i);
  });

  it("fails when the server has no build for this platform", async () => {
    const { fetchImpl } = fakeServer({ keys: ["darwin-arm64"] });
    await expect(downloadServerCli({ ...base, fetchImpl })).rejects.toThrow(/linux-amd64/);
  });

  it("fails without a server URL, and never reaches the network", async () => {
    const { fetchImpl, calls } = fakeServer();
    await expect(downloadServerCli({ ...base, serverUrl: "", fetchImpl })).rejects.toThrow(/server/i);
    expect(calls).toHaveLength(0);
  });

  it("fails clearly when the server rejects the token", async () => {
    const { fetchImpl } = fakeServer({ status: 401 });
    await expect(downloadServerCli({ ...base, fetchImpl })).rejects.toThrow(/401/);
  });
});
