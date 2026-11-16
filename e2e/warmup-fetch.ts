/**
 * One warm-up request for the Playwright global setup. Never throws: a failed
 * or slow route only means that route stays cold. No imports, so a plain Node
 * test can load it (scripts/e2e-warmup-timeout.test.mjs).
 */
export async function warmFetch(url: string, _timeoutMs: number): Promise<void> {
  await fetch(url, {
    headers: { cookie: "multica_logged_in=1" },
    redirect: "manual",
  }).catch(() => undefined);
}
