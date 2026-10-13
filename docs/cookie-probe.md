# Temporary Brave session-cookie probe

This is step 1 of CHE-1593, based on layer B (PR #219). Live suggestions are
not implemented. Only Cheese activates the shortcut in Brave. Presence of a
`multica_auth` cookie is not proof that the cookie authenticates a user.

## Deployment

The app route and OpenSearch suggestions URL are disabled unless
`MULTICA_COOKIE_PROBE=1`. The standalone server uses those same route handlers,
without a database, authentication changes, private search or access logging.
It retains only aggregate counts of the cookie-presence booleans. Restarting
the process clears the counts. Do not enable header/body tracing or access logs.

Build on X99 using the already-installed esbuild dependency:

```sh
node_modules/.pnpm/esbuild@0.25.12/node_modules/esbuild/bin/esbuild scripts/cookie-probe.ts --bundle --platform=node --format=esm --outfile=cookie-probe.mjs
sha256sum cookie-probe.mjs
```

The C00 owner serves the digest-verified artifact with Node 22 or newer, on an
unused loopback port behind a dedicated private HTTPS listener. Do not change
the production app or its existing listeners. Use the same hostname as Cheese's
existing Multica session, so this probe exercises the real session cookie.
Verify the cookie's host/path eligibility without printing its value. An X99
origin or unrelated test cookie is not acceptance evidence for the C00 session.

```sh
MULTICA_COOKIE_PROBE=1 COOKIE_PROBE_ORIGIN=https://congvc-c00.taila6fa8a.ts.net:19693 COOKIE_PROBE_PORT=19693 node cookie-probe.mjs
```

Keep the server under a host-owned supervisor, with durable boolean-only logs.
Record the unit/PID, URL, artifact digest and exact listener cleanup command.
Check the root page, OpenSearch XML and `/probe-status` before asking Cheese.
Do not GET the suggestions URL during Cheese's measurement.

## Cheese's test (about two minutes)

1. Sign in to the usual Multica instance in Brave. Open the verified probe URL
   in another tab. Record the `withCookie` and `withoutCookie` counts.
2. Open `brave://settings/searchEngines`. Activate the probe origin's Multica
   entry under Inactive shortcuts. Use the shortcut `mcprobe`. If a stale entry
   exists for this origin, remove only that probe entry and revisit the probe.
3. Type `mcprobe`, press Tab, and type `cookieprobe` without pressing Enter.
   Return to the probe tab and compare the counts. If neither count changes,
   also record whether Brave showed suggestions enabled.
4. Report **no fetch**, **fetch without cookie**, or **fetch with cookie**,
   the before/after counts, the Brave version from `brave://version`, and the
   date. Never paste a cookie or an unredacted HAR.

No change to either count means no fetch. An increase in only `withoutCookie`
means fetch without cookie. An increase in `withCookie` means fetch with cookie.
If both increase, report both counts; do not silently classify mixed evidence.

## Stop gate and removal

No fetch or fetch without cookie: stop, remove the temporary suggestions URL,
handler and standalone runner in the same PR, stop the supervised probe, and
remove only its dedicated listener. Report the outcome on CHE-1593. No tokens
in URLs or other authentication workaround.

Fetch with cookie: retain the outcome, version and date before implementing
live suggestions with auth, workspace/session-user scoping, rate limiting,
logged-out tests and a live-suggestion screenshot. Write the failing test first.
The probe is not a live endpoint and the PR stays draft until the remaining
scope and independent cross-lab review are complete. Agents never merge.
