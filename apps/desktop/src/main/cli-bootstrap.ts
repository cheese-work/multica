import { app } from "electron";
import { execFile } from "child_process";
import { createHash } from "crypto";
import { existsSync } from "fs";
import { chmod, mkdir, rename, rm, writeFile } from "fs/promises";
import { join, dirname } from "path";

// Desktop prefers the bundled `multica` CLI shipped inside the app for
// same-repo builds, but it can also repair or bootstrap a managed copy in
// userData on first launch when the bundled binary is missing or unusable.
//
// The managed copy comes from the Multica server the app is connected to
// (GET /api/daemon/update + /api/daemon/update/binary, CHE-2641), the same
// source `multica update` and the daemon use. Never from upstream releases.

function binaryName(): string {
  return process.platform === "win32" ? "multica.exe" : "multica";
}

export function managedCliPath(): string {
  return join(app.getPath("userData"), "bin", binaryName());
}

function run(cmd: string, args: string[], cwd?: string): Promise<void> {
  return new Promise((resolve, reject) => {
    execFile(cmd, args, { cwd }, (err) => (err ? reject(err) : resolve()));
  });
}

const NODE_TO_GO_OS: Record<string, string> = { darwin: "darwin", linux: "linux" };
const NODE_TO_GO_ARCH: Record<string, string> = { x64: "amd64", arm64: "arm64" };

/** The server's build key (`linux-amd64`) for a node platform/arch. */
export function serverCliKey(
  platform: string = process.platform,
  arch: string = process.arch,
): string {
  const os = NODE_TO_GO_OS[platform];
  const goArch = NODE_TO_GO_ARCH[arch];
  if (!os || !goArch) {
    throw new Error(
      `unsupported platform for CLI auto-install: ${platform}/${arch} (the server builds linux and darwin on amd64/arm64)`,
    );
  }
  return `${os}-${goArch}`;
}

async function serverGet(
  fetchImpl: typeof fetch,
  serverUrl: string,
  path: string,
  token: string,
): Promise<Response> {
  const res = await fetchImpl(`${serverUrl}${path}`, {
    redirect: "follow",
    headers: token ? { Authorization: `Bearer ${token}` } : {},
  });
  if (!res.ok) {
    throw new Error(`${path} failed: HTTP ${res.status}`);
  }
  return res;
}

/**
 * Downloads the server's own CLI build for this platform and verifies it
 * against the sha256 in the server's manifest before returning the bytes.
 */
export async function downloadServerCli(opts: {
  serverUrl: string;
  token?: string;
  platform?: string;
  arch?: string;
  fetchImpl?: typeof fetch;
}): Promise<Buffer> {
  const serverUrl = opts.serverUrl.trim().replace(/\/+$/, "");
  if (!/^https?:\/\/./.test(serverUrl)) {
    throw new Error("no Multica server URL to download the CLI from");
  }
  const key = serverCliKey(opts.platform, opts.arch);
  const token = opts.token ?? "";
  const fetchImpl = opts.fetchImpl ?? fetch;

  const manifest = (await (
    await serverGet(fetchImpl, serverUrl, "/api/daemon/update", token)
  ).json()) as { binaries?: Record<string, { sha256?: string }> };
  const expected = manifest.binaries?.[key]?.sha256?.toLowerCase();
  if (!expected || !/^[0-9a-f]{64}$/.test(expected)) {
    throw new Error(`the server has no ${key} CLI build`);
  }

  const [os, arch] = key.split("-");
  const bytes = Buffer.from(
    await (
      await serverGet(
        fetchImpl,
        serverUrl,
        `/api/daemon/update/binary?os=${os}&arch=${arch}`,
        token,
      )
    ).arrayBuffer(),
  );
  const actual = createHash("sha256").update(bytes).digest("hex");
  if (actual !== expected) {
    throw new Error(
      `checksum mismatch for ${key}: expected ${expected}, got ${actual} — refusing to install unverified binary`,
    );
  }
  return bytes;
}

export interface EnsureManagedCliOptions {
  forceInstall?: boolean;
  /** Base URL of the Multica server to download from. */
  serverUrl?: string | null;
  /** Credential for `serverUrl` (the update endpoints sit behind daemon auth). */
  token?: string;
}

async function installFresh(
  serverUrl: string | null | undefined,
  token: string | undefined,
): Promise<string> {
  const target = managedCliPath();
  const bytes = await downloadServerCli({ serverUrl: serverUrl ?? "", token });

  console.log(`[cli-bootstrap] downloaded ${bytes.length} bytes from ${serverUrl}`);
  await mkdir(dirname(target), { recursive: true });
  const staged = `${target}.new`;
  await writeFile(staged, bytes, { mode: 0o755 });
  try {
    await chmod(staged, 0o755);
    await rm(target, { force: true }).catch(() => {});
    await rename(staged, target);
  } finally {
    await rm(staged, { force: true }).catch(() => {});
  }

  // macOS: ad-hoc sign so spawning the child never hits a gatekeeper quirk.
  // Non-fatal: unsigned binaries still execute when the parent app is trusted.
  if (process.platform === "darwin") {
    await run("codesign", ["-s", "-", "--force", target]).catch((err) => {
      console.warn("[cli-bootstrap] ad-hoc codesign failed:", err);
    });
  }

  console.log(`[cli-bootstrap] installed CLI at ${target}`);
  return target;
}

/**
 * Returns the path to a usable `multica` binary. If one is already present at
 * the managed userData location, returns it immediately. Otherwise downloads
 * the server's build for the current platform and installs it.
 */
export async function ensureManagedCli(
  options: EnsureManagedCliOptions = {},
): Promise<string> {
  const target = managedCliPath();
  if (existsSync(target) && !options.forceInstall) return target;
  return installFresh(options.serverUrl, options.token);
}
