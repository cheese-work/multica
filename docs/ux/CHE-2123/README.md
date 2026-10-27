## Candidate

Runtime source: `0bb2d04d80ca3b689986b5badbe33d7e3d16c87d`.
Approved design: `design/viewer-media-pages/rev-1/A.html` @ `13613770066f99d1e8389e73fe877d11a20c4ee0`.

The capture build uses real product Electron main IPC, preload, routing, and shared viewers. Only daemon-manager and updater startup are stubbed. The build does not start or modify the workspace daemon. Web uses the real Next application. Both use a synthetic local API with a fixture user, workspace, issue, and four persisted media items. No real account data or credentials are used.

This is frontend fixture evidence, not a backend authorization acceptance run. The fixture returns minimal responses for unrelated endpoints and does not implement WebSocket upgrades. The resulting schema and WebSocket warnings are fixture limitations. Earlier web logs also include sandbox localStorage errors from the capture initializer running inside opaque iframes; the checked-in initializer catches that expected denial.

## Reproduce On X99

Run from the repository root after the normal `pnpm install --frozen-lockfile`:

```bash
VITE_API_URL=http://127.0.0.1:31954 \
VITE_APP_URL=http://127.0.0.1:31953 \
VITE_WS_URL=ws://127.0.0.1:31954/ws \
pnpm --filter @multica/desktop exec electron-vite build \
  --config ../../docs/ux/CHE-2123/capture.config.ts

node docs/ux/CHE-2123/capture.mjs
```

The harness needs `/usr/bin/google-chrome` and the installed Electron binary. Electron uses its headless native platform on X99; neither a physical device nor a MacBook is required. Ports 31953 and 31954 must be free. `CAPTURE_WEB=0` or `CAPTURE_DESKTOP=0` selects one surface. Output goes to `evidence/che-2123/captures/`.

The harness closes its own browser, native windows, fixture API, and Next process group in `finally`. Its isolated Electron profile is `/tmp/che-2123-capture-home`. The harness is a manual evidence tool, not a persistent preview service.

## Checks

Committed console transcripts remove trailing whitespace and final blank lines only. Original run output remains in the task's local `evidence/che-2123/` directory. No result or behavioral assertion is reconstructed.

- `logs/permission-red.txt` and `logs/permission-green.txt`: observed test-first behavioral failure and pass for cached-media suppression on HTTP 403. This test targets the existing attachment route, not a new server endpoint. Parent: `13613770066f99d1e8389e73fe877d11a20c4ee0`.
- `logs/more-escape-red.txt` and `logs/more-escape-green.txt`: observed failure and pass when a nested toolbar action consumes Escape. Parent: `335590f233c1eae9a170b4da2fd540e01f0ae690`.
- Viewer suite: 405 tests in 11 files. Desktop suite: 76 tests in 5 files. Core suite: 8 tests. Total: 489 tests.
- Views, web, and desktop typechecks pass. Focused views ESLint reports no warnings or errors. Full repository suites and backend integration are NOT-RUN locally.
- Web: four kinds at 1440 and 390 px, reload, copy-link, native Enter/Tab/Escape path, and denied image route with zero additional fixture-image requests.
- Desktop: actual `openMediaWindow` IPC, four native windows at 960 by 760, reload, and public copy-link values.

## Draft Hold

The new media route has no baseline page. Source-dialog before/after captures at 1440 and 390 px remain NOT-CAPTURED. The UI review pack is incomplete. Independent code and UX review remain pending. Do not mark the PR ready until those gates are satisfied.

Visual diff: NOT-RUN (CHE-1485 Layer B pending).

## Cleanup Receipt

Initial free space before task writes: workspace 127366287360 bytes; `/tmp` 175870824448 bytes. The host is shared, so whole-run free-space changes are not attributable to this task alone.

```text
Cleanup receipt
- Mount: workspace; df -B1 before: 108335644672; after: 109004234752; net returned: 668590080 bytes.
- Mount: /tmp; df -B1 before: 182409699328; after: 182413565952; net returned: 3866624 bytes.
- Removed (exact paths): apps/web/.next; apps/desktop/out; /tmp/che-2123-capture-home; electron-capture-probe.cjs.
- Reference check: capture sessions completed; ports 31953 and 31954 returned ECONNREFUSED; git worktree list showed only the task checkout.
- Recoverable because: generated Next/Electron builds and the isolated capture profile can be recreated with the documented commands; the probe is superseded by the committed harness.
- Preserved on purpose: evidence, unrelated reference checkouts, task-local dependencies for pending review/corrections, shared pnpm and Electron caches.
- Health after: multica daemon status reported running; no daemon restart or reload was performed.
- Residual / owner decision: task-local node_modules is about 1.9 GiB; x99-codex-sol retains it for pending author corrections. No human decision is required.
```

The net figures are observed shared-mount free-space changes during cleanup, not sums of nominal file sizes.
