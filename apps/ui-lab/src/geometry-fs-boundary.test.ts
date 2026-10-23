// Runtime check for the geometry harness's Vite file-serving boundary (CHE-1829).
// Starts the harness server on loopback with no browser and no database, asks
// for a file outside the workspace and for files the harness really imports.
import { mkdtemp, realpath, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { createServer, type ViteDevServer } from "vite";

const here = (path: string) => fileURLToPath(new URL(path, import.meta.url));

let server: ViteDevServer | undefined;
let origin = "";
let outsideDir = "";
let outsideFile = "";

beforeAll(async () => {
  outsideDir = await mkdtemp(join(tmpdir(), "geometry-fs-boundary-"));
  outsideFile = join(outsideDir, "secret.txt");
  await writeFile(outsideFile, "outside the workspace");
  server = await createServer({
    configFile: here("../geometry/vite.config.ts"),
    server: { port: 0, host: "127.0.0.1" },
    logLevel: "error",
  });
  await server.listen();
  origin = server.resolvedUrls!.local[0]!;
}, 60_000);

afterAll(async () => {
  await server?.close();
  if (outsideDir) await rm(outsideDir, { recursive: true, force: true });
});

const raw = (file: string) => fetch(`${origin}@fs${file}?raw`);

describe("geometry harness file serving", () => {
  it("refuses a file outside the workspace", async () => {
    const response = await raw(await realpath(outsideFile));

    expect(response.status).toBe(403);
    expect(await response.text()).not.toContain("outside the workspace");
  });

  it.each([
    ["the rich-content entry", "../../../packages/views/rich-content/index.ts"],
    ["the harness stylesheet", "./styles.css"],
    ["a hoisted dependency", "../node_modules/react/package.json"],
  ])("still serves %s", async (_name, path) => {
    const response = await raw(await realpath(here(path)));

    expect(response.status).toBe(200);
  });
});
