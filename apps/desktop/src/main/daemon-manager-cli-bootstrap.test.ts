// @vitest-environment node
import { beforeEach, afterEach, describe, expect, it, vi } from "vitest";
import { deriveProfileName, profileConfigPath } from "./daemon-profile";

type IpcHandler = (...args: unknown[]) => unknown;

const SERVER = "https://multica.example.test";
const CONFIG_PATH = profileConfigPath(deriveProfileName(SERVER));

const ctx = vi.hoisted(() => ({
  ipc: new Map<string, IpcHandler>(),
  files: new Map<string, string>(),
  statuses: [] as string[],
  downloads: [] as Array<{ serverUrl?: string | null; token?: string }>,
  failDownload: false,
  // When set, the download reports this progress and then never finishes.
  stalledProgress: null as { bytesDone: number; bytesTotal: number } | null,
}));

vi.mock("electron", () => ({
  app: {
    on: vi.fn(),
    getPath: () => "/review-only/user-data",
    getAppPath: () => "/review-only/app",
    getVersion: () => "0.7.0",
    getPreferredSystemLanguages: () => ["en"],
  },
  BrowserWindow: class {},
  ipcMain: {
    handle: (name: string, fn: IpcHandler) => ctx.ipc.set(name, fn),
    on: vi.fn(),
  },
  shell: { openPath: vi.fn() },
}));

// Mirrors the real contract: the server's update endpoints need a URL and a token.
vi.mock("./cli-bootstrap", () => ({
  managedCliPath: () => "/review-only/managed-cli",
  ensureManagedCli: vi.fn(
    async (opts: {
      serverUrl?: string | null;
      token?: string;
      onProgress?: (p: { bytesDone: number; bytesTotal: number }) => void;
    }) => {
      ctx.downloads.push({ serverUrl: opts.serverUrl, token: opts.token });
      if (ctx.stalledProgress) {
        opts.onProgress?.(ctx.stalledProgress);
        return new Promise<string>(() => {});
      }
      if (ctx.failDownload || !opts.serverUrl || !opts.token) {
        throw new Error("authenticated CLI download failed");
      }
      return "/review-only/managed-cli";
    },
  ),
}));

// No bundled, managed or PATH CLI exists on this machine.
vi.mock("fs", () => ({
  existsSync: () => false,
  watchFile: vi.fn(),
  unwatchFile: vi.fn(),
}));
vi.mock("fs/promises", () => ({
  readFile: vi.fn(async (path: string) => {
    const body = ctx.files.get(path);
    if (body === undefined) {
      throw Object.assign(new Error("ENOENT"), { code: "ENOENT" });
    }
    return body;
  }),
  writeFile: vi.fn(async (path: string, body: string) => {
    ctx.files.set(path, body);
  }),
  mkdir: vi.fn(),
  rm: vi.fn(),
  open: vi.fn(),
  stat: vi.fn(),
}));
vi.mock("./daemon-port", () => ({
  probeProfileHealth: vi.fn(async () => null),
}));
vi.mock("child_process", () => ({
  execFile: vi.fn(
    (
      _file: string,
      args: string[],
      _opts: unknown,
      cb: (err: Error | null, stdout: string) => void,
    ) => cb(null, args[0] === "version" ? '{"version":"0.7.0"}' : ""),
  ),
}));

const call = (name: string, ...args: unknown[]) =>
  ctx.ipc.get(name)!({}, ...args);

async function startManager() {
  const manager = await import("./daemon-manager");
  manager.setupDaemonManager(() => ({
    isDestroyed: () => false,
    webContents: { send: (_c: string, s: { state: string }) => ctx.statuses.push(s.state) },
  }) as never);
  // Let the startup bootstrap run before the renderer reports anything.
  await vi.advanceTimersByTimeAsync(0);
}

beforeEach(() => {
  vi.resetModules();
  vi.useFakeTimers();
  ctx.ipc.clear();
  ctx.files.clear();
  ctx.statuses.length = 0;
  ctx.downloads.length = 0;
  ctx.failDownload = false;
  ctx.stalledProgress = null;
});
afterEach(() => {
  vi.clearAllTimers();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("CLI bootstrap waits for its download prerequisites", () => {
  it("does not download or give up when startup precedes the server URL", async () => {
    await startManager();
    expect(ctx.downloads).toHaveLength(0);
    expect(ctx.statuses).not.toContain("cli_not_found");
    await expect(call("daemon:is-cli-installed")).resolves.toBe(false);
    expect(ctx.downloads).toHaveLength(0);
  });

  it("does not download or give up when the URL arrives before the PAT", async () => {
    await startManager();
    await call("daemon:set-target-api-url", SERVER);
    await vi.advanceTimersByTimeAsync(0);
    expect(ctx.downloads).toHaveLength(0);
    expect(ctx.statuses).not.toContain("cli_not_found");
  });

  it("installs once the PAT arrives after the URL", async () => {
    await startManager();
    await call("daemon:set-target-api-url", SERVER);
    await call("daemon:sync-token", "mul_pat_from_login", "user-1");
    await vi.advanceTimersByTimeAsync(0);
    expect(ctx.downloads).toEqual([{ serverUrl: SERVER, token: "mul_pat_from_login" }]);
    await expect(call("daemon:is-cli-installed")).resolves.toBe(true);
    expect(ctx.statuses).not.toContain("cli_not_found");
    expect(ctx.statuses.at(-1)).toBe("stopped");
  });

  it("installs when the URL arrives and a PAT is already on disk", async () => {
    ctx.files.set(CONFIG_PATH, JSON.stringify({ server_url: SERVER, token: "mul_saved_pat" }));
    await startManager();
    expect(ctx.downloads).toHaveLength(0);
    await call("daemon:set-target-api-url", SERVER);
    await vi.advanceTimersByTimeAsync(0);
    expect(ctx.downloads).toEqual([{ serverUrl: SERVER, token: "mul_saved_pat" }]);
    await expect(call("daemon:is-cli-installed")).resolves.toBe(true);
  });

  it("still reports cli_not_found when a download with both prerequisites fails", async () => {
    ctx.failDownload = true;
    await startManager();
    await call("daemon:set-target-api-url", SERVER);
    await call("daemon:sync-token", "mul_pat_from_login", "user-1");
    await vi.advanceTimersByTimeAsync(0);
    expect(ctx.downloads).toHaveLength(1);
    expect(ctx.statuses.at(-1)).toBe("cli_not_found");
    await expect(call("daemon:is-cli-installed")).resolves.toBe(false);
  });

  it("reports download progress in the polled status when a retry follows cli_not_found", async () => {
    ctx.failDownload = true;
    await startManager();
    await call("daemon:set-target-api-url", SERVER);
    await call("daemon:sync-token", "mul_pat_from_login", "user-1");
    await vi.advanceTimersByTimeAsync(0);
    expect(ctx.statuses.at(-1)).toBe("cli_not_found");

    ctx.failDownload = false;
    ctx.stalledProgress = { bytesDone: 5, bytesTotal: 20 };
    void call("daemon:retry-install");
    await vi.advanceTimersByTimeAsync(0);

    await expect(call("daemon:get-status")).resolves.toEqual({
      state: "installing_cli",
      cliDownload: { bytesDone: 5, bytesTotal: 20 },
    });
  });
});
