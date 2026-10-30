import { chromium, type FullConfig } from "@playwright/test";
import { TestApiClient } from "./fixtures";

/**
 * The suite runs against `next dev`, which compiles a route on its first
 * visit: 10 to 20 seconds for a dashboard page. A spec that happens to be the
 * first to open a route then waits on the compiler, not on the app, and its
 * click or URL assertion times out. Visit each route once, up front.
 *
 * The fetch pass compiles each route's server bundle; it never runs the page,
 * so the client chunks (the dashboard layout, the search worker) stay cold and
 * the first spec to open a workspace waits on them for 30s or more. The browser
 * pass loads every route as a signed-in member to compile those too.
 */
const ROUTES = [
  "login",
  "onboarding",
  "e2e-warmup/issues",
  "e2e-warmup/issues/E2EW-1",
  "e2e-warmup/my-issues",
  "e2e-warmup/inbox",
  "e2e-warmup/chat",
  "e2e-warmup/projects",
  "e2e-warmup/projects/00000000-0000-4000-8000-000000000000",
  "e2e-warmup/agents",
  "e2e-warmup/agents/00000000-0000-4000-8000-000000000000",
  "e2e-warmup/squads",
  "e2e-warmup/skills",
  "e2e-warmup/runtimes",
  "e2e-warmup/autopilots",
  "e2e-warmup/settings",
  "e2e-warmup/search",
  "auth/callback",
];

const WARMUP_SLUG = "e2e-warmup";

async function warmClientChunks(baseURL: string) {
  const api = new TestApiClient();
  await api.login(`e2e-warmup-${Date.now().toString(36)}@multica.ai`, "E2E Warmup");
  await api.ensureWorkspace("E2E Warmup", WARMUP_SLUG);
  await api.markUserOnboarded();
  const token = api.getToken();
  if (!token) return;

  const browser = await chromium.launch();
  try {
    const context = await browser.newContext();
    await context.addInitScript((t) => localStorage.setItem("multica_token", t), token);
    await Promise.all(
      ROUTES.filter((route) => route.startsWith(`${WARMUP_SLUG}/`)).map(async (route) => {
        const page = await context.newPage();
        await page
          .goto(`${baseURL}/${route}`, { waitUntil: "networkidle", timeout: 180_000 })
          .catch(() => undefined);
        await page.close();
      }),
    );
  } finally {
    await browser.close();
  }
}

export default async function globalSetup(config: FullConfig) {
  const baseURL = config.projects[0]?.use.baseURL;
  if (!baseURL || !/^https?:\/\/(localhost|127\.0\.0\.1)(:|\/|$)/.test(baseURL)) return;
  await Promise.all(
    ROUTES.map((route) =>
      fetch(`${baseURL}/${route}`, {
        headers: { cookie: "multica_logged_in=1" },
        redirect: "manual",
      }).catch(() => undefined),
    ),
  );
  await warmClientChunks(baseURL).catch(() => undefined);
}
