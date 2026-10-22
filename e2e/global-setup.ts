import type { FullConfig } from "@playwright/test";

/**
 * The suite runs against `next dev`, which compiles a route on its first
 * visit: 10 to 20 seconds for a dashboard page. A spec that happens to be the
 * first to open a route then waits on the compiler, not on the app, and its
 * click or URL assertion times out. Visit each route once, up front.
 *
 * Pages render on the client, so no login is needed beyond the session cookie
 * the proxy checks before it lets a workspace URL through.
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
}
