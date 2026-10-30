## Candidate

Captured source: `6aa9943b2680749832c336fa2cd84050fb40ae0e`.
Approved design: `design/viewer-media-pages/rev-1/A.html` @ `13613770066f99d1e8389e73fe877d11a20c4ee0`.

The current correction preserves Open for saved description blocks and hides it for unsaved blocks and composer content. The editor and route resolver use the same markdown-media parser. All four source dialogs keep Close outside phone More. Earlier corrections cover closed-summary Escape, normal not-found recovery, translated window titles, and import placement.

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

CAPTURE_SOURCE_PHASE=after node docs/ux/CHE-2123/capture.mjs
```

The harness needs `/usr/bin/google-chrome` and the installed Electron binary. Electron uses its headless native platform on X99; neither a physical device nor a MacBook is required. Ports 31953 and 31954 must be free. `CAPTURE_WEB=0` or `CAPTURE_DESKTOP=0` selects one surface. Output goes to `evidence/che-2123/captures/`. `CAPTURE_OUTPUT` overrides that directory.

For the before captures, run this revision's harness with the baseline checkout as the working directory. Pin the baseline checkout to `13613770066f99d1e8389e73fe877d11a20c4ee0`. Set `CAPTURE_SOURCE_PHASE=before CAPTURE_MEDIA=0 CAPTURE_DESKTOP=0`. The fixture includes comment media and saved Mermaid/HTML issue-description blocks. The harness reads both comment and timeline endpoints. The after phase separately opens the saved description blocks through the real ContentEditor and checks their popup URLs.

Source pages use the canonical `CAP-1` issue URL. The fixture accepts that identifier and the source ID. This avoids a canonical-route replacement during capture. The harness waits for actual SVG/HTML/image/table content. Reduced motion and disabled screenshot animations avoid recording a dialog's transparent entry frame.

The harness closes its own browser, native windows, fixture API, and Next process group in `finally`. Its isolated Electron profile is `/tmp/che-2123-capture-home`. The harness is a manual evidence tool, not a persistent preview service.

## Capture Matrix

After source: `6aa9943b2680749832c336fa2cd84050fb40ae0e`. Before source: `13613770066f99d1e8389e73fe877d11a20c4ee0`. Subsequent commits in this fix cycle contain documentation, logs, and PNGs only. Verify runtime-source parity with `git diff --exit-code 6aa9943b2680749832c336fa2cd84050fb40ae0e HEAD -- apps packages`.

| Surface | Width | Before | After |
| --- | --- | --- | --- |
| Web Mermaid / table / image / HTML | 1440 / 390 | Not applicable: new route | `captures/web-{kind}-{width}-after.png` |
| Desktop Mermaid / table / image / HTML | 960 | Not applicable: new route | `captures/desktop-{kind}-960-after.png` |
| Source Mermaid / table / image / HTML dialogs | 1440 / 390 | `captures/source-{kind}-{width}-before.png` | `captures/source-{kind}-{width}-after.png` |
| Saved description Mermaid / HTML | 1440 / 390 | Dialog baseline above | `captures/description-{kind}-{width}-after.png` |
| Denied web route | 1440 | Not applicable: new route | `captures/web-denied-1440.png` |

Contact sheets: `source-dialogs-1440-contact-sheet.png`, `source-dialogs-390-contact-sheet.png`, `saved-description-contact-sheet.png`, `web-contact-sheet.png`, and `desktop-contact-sheet.png`. Each sheet lives in `captures/`. The image fixture is the approved design board PNG. Apparent app chrome inside that image is image content, not the surrounding media route.

Manual Keep / Change / Remove check:
- Keep: shared media toolbar and stage, source dialog dismissal, sandboxed HTML/diagram documents, and existing viewer tools.
- Change: persisted source Open actions resolve actual media-page popups; standalone Copy link and reload pass; phone More has Enter/Tab/Escape paths.
- Remove: none from source dialogs. Close stays visible outside More at 390 px for Mermaid, table, image, and HTML. Standalone media pages intentionally omit issue/comments/app shell/Close.

The current web-only and desktop-only runs both completed with exit 0. `web-receipt.json` and `desktop-receipt.json` are unmodified successful harness receipts at the captured source. Web covers all four source dialogs at 1440/390, saved Mermaid/HTML description popups at both widths, standalone pages at both widths, reload/copy-link, phone keyboard paths, and denied-page zero-media requests. Desktop covers native IPC windows for all four kinds, reload, and public copy links. Logs: `logs/review2-{web,desktop}-capture.txt`.

The saved-description capture expands the collapsed description and uses focus/Enter to open each preview. Mermaid's unchanged inline opener overlaps the editor's Copy control under pointer activation. No pointer-activation PASS or repair is claimed for that opener. The first provisional capture stopped at the collapsed block; the second stopped at the overlapping control. Only the completed keyboard-path run supplies final after PNGs. Comment-dialog Open buttons and saved-description Open buttons both resolve the expected popup URL.

The baseline full capture receipt is `source-before-receipt.json`. A transparent table-dialog entry frame at 1440 required a reduced-motion recapture. The replacement has its own `source-table-before-receipt.json`. Other provisional failures involved missing root TypeScript links, canonical-route timing, and a missing required fixture comment `type`. Provisional screenshots do not serve as final evidence.

Desktop emits security-configuration warnings and an expected missing daemon IPC handler in the isolated capture build. These warnings are retained in the raw log. Capture success does not certify desktop hardening. Preview: not available. Independent UX verdict: pending.

Desktop's version metadata includes a dirty marker from uncommitted documentation/evidence. `git diff --exit-code 6aa9943b2680749832c336fa2cd84050fb40ae0e HEAD -- apps packages` is empty. No product source changed after capture.

## Checks

Committed console transcripts remove trailing whitespace and final blank lines only. Original run output remains in the task's local `evidence/che-2123/` directory. No result or behavioral assertion is reconstructed.

- `logs/permission-red.txt` and `logs/permission-green.txt`: observed test-first behavioral failure and pass for cached-media suppression on HTTP 403. This test targets the existing attachment route, not a new server endpoint. Parent: `13613770066f99d1e8389e73fe877d11a20c4ee0`.
- `logs/more-escape-red.txt` and `logs/more-escape-green.txt`: observed failure and pass when a nested toolbar action consumes Escape. Parent: `335590f233c1eae9a170b4da2fd540e01f0ae690`.
- Original new-route permission tests in `media-page.test.tsx` and `resolve-media.test.ts` are **retrospective**. No behavioral red was observed for those tests. The attachment-route red does not satisfy the new-route requirement. Only Cheese can waive this run-practice gap. No waiver is claimed or requested in this draft correction.
- Current review rerun: 184 tests in 12 files; views/web/desktop typechecks pass. Scoped ESLint passes with zero warnings outside the unchanged content-editor warning. The 404-display regression below does not supply authorization-red evidence.
- Earlier execution: viewer suite 405 tests in 11 files; desktop suite 76 tests in 5 files; core suite 8 tests. Those counts are retained history, not a rerun on the review-corrected source.
- Full changed-file ESLint reports one pre-existing `react-hooks/exhaustive-deps` warning for `inFlightUploadsRef.current` in `content-editor.tsx`. Running ESLint on that file from parent `25da33c992dd409c1ffd0cf1c9d1733b99612c08` reports the same warning. No budget increase or suppression is added. Logs: `review2-lint.txt`, `review2-lint-baseline.txt`, and `review2-lint-scoped.txt`. Full repository suites and backend integration are NOT-RUN locally.
- Web: four kinds at 1440 and 390 px, reload, copy-link, native Enter/Tab/Escape path, and denied image route with zero additional fixture-image requests.
- Desktop: actual `openMediaWindow` IPC, four native windows at 960 by 760, reload, and public copy-link values.

## Review Correction: Observed Red / Green

Parent: `ee28bd4980c424531223b63857276aba02bec64d`. Each test was added and observed failing before its corresponding implementation change. The same focused command then passed. Each log pair is in `logs/review-{slice}-{red,green}.txt`.

| Slice | Focused views test command suffix | Observed red, exit 1 | Observed green, exit 0 |
| --- | --- | --- | --- |
| `escape` | `media/media-context.test.tsx -t 'closed More'` | Document listener called 0 times, expected once; 1 failed | 1 passed |
| `not-found` | `media/media-page.test.tsx -t 'normal not-found'` | Missing `404` text; 1 failed | 1 passed |
| `draft` | `editor/extensions/code-block-view.test.tsx -t 'unsaved HTML'` | Draft media-link action was not null; 1 failed | 1 passed |
| `title` | `media/media-context.test.tsx -t 'readable title'` | 4 failed, 1 already passed | 5 passed |

Prefix each suffix with `pnpm --filter @multica/views test`. The editor boundary clears persisted media context for both HTML and Mermaid drafts. HTML retains its preview without an invalid persisted-media action. The title test covers Mermaid, table, image, HTML, and attachment.

The import-only edits have no behavioral-red claim. `review-suite-green.txt`, `review-lint.txt`, and `review-*-typecheck.txt` retain the current validation output.

## Second Review Correction: Observed Red / Green

Parent: `25da33c992dd409c1ffd0cf1c9d1733b99612c08`. Both test files were written and run before implementation. Prefix each command suffix with `pnpm --filter @multica/views exec vitest run`.

| Fix | Command suffix | Observed red, exit 1 | Observed green, exit 0 |
| --- | --- | --- | --- |
| Saved description links | `editor/content-editor-media.test.tsx --reporter=verbose` | 2 failed, 2 passed. Saved Open is missing; edit/undo case also fails at its initial saved-action assertion. Unsaved and composer checks already pass. | 4 passed, including saved popup, unsaved block, edit/undo, and composer checks. |
| Visible phone Close | `media/media-dialog-close.test.tsx --reporter=verbose` | 3 behavioral failures: Mermaid/table/HTML Close is inside details, expected null. Image also fails, but at API fixture setup, not the behavior. | 4 passed. Every dialog Close is outside More, focusable, and dismisses without opening More. |

Raw logs: `logs/review2-{description,close}-{red,green}.txt`. The image setup was repaired after implementation, so that image assertion is retrospective; no image-specific behavioral red is claimed. The combined green suite is `logs/review2-suite-green.txt` (184 tests in 12 files). Failed intermediate green attempts remain local and are not presented as passes.

Code-read-only optional finding: desktop `MediaUnavailablePage` still links to `/`, and the issue-window navigation adapter opens that destination in the browser. Its `min-h-screen` is inside the frame's remaining height below DragStrip. This optional behavior is unchanged. A denied desktop-window recovery/overflow capture is NOT-RUN; no scroll outcome is claimed.

## Draft Hold

Independent code and UX review remain pending. The original new-route test-first gap remains open. The leader requests a Cheese waiver only at ready time. Do not mark the PR ready in this fix cycle. No merge or deploy is requested.

Visual diff: NOT-RUN (CHE-1485 Layer B pending).

## Warning-Budget Follow-Up

Parent: `67cecb3eeb2a62ec68ebffb6bf4fc93aac66334f`. CI run `40500415876` reported one new `react-hooks/exhaustive-deps` warning in `issue-window.tsx` (baseline 0, current 1). The focused local reproduction identifies the missing dependency as `context.kind`. The fix adds that dependency; no warning budget or lint configuration changes.

Command before and after the fix:

```bash
pnpm --filter @multica/desktop exec eslint src/renderer/src/components/issue-window.tsx --max-warnings 0
```

Observed lint failure: exit 1, one missing-`context.kind` warning. Observed lint pass: exit 0, no warnings. Raw command output is retained in `logs/desktop-hook-lint-red.txt` and `logs/desktop-hook-lint-green.txt`. This is lint evidence, not a new behavioral-red claim. The original permission behavioral-red/green receipt remains above.

The focused desktop rerun passes 33 tests in 5 files: `issue-window.test.tsx`, shared `issue-window.test.ts`, `issue-window-navigation.test.tsx`, `App.auth-recovery.test.tsx`, and `auth-session-coordinator.test.ts`.

`pnpm --filter @multica/desktop typecheck` also passes both the main/preload and renderer checks. `git diff --check` passes.

## Earlier Cleanup Receipt

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

## Review-Correction Cleanup Receipt

```text
Cleanup receipt
- Mount: workspace; df -B1 before: 314568343552; after: 316216238080; net returned: 1647894528 bytes.
- Mount: /tmp; df -B1 before: 141650382848; after: 141356769280; net returned: -293613568 bytes.
- Removed (exact paths): ../che-2123-ui-baseline worktree; apps/web/.next; apps/desktop/out; /tmp/che-2123-capture-home.
- Reference check: all capture sessions collected; ports 31953 and 31954 returned ECONNREFUSED; git worktree list contains only the task checkout after cleanup.
- Recoverable because: the detached baseline is pinned, and generated Next/Electron outputs and the isolated profile are reproducible with the documented harness.
- Preserved on purpose: published captures and transcripts, local raw/provisional evidence, task dependencies, shared caches, and unrelated reference checkouts.
- Health after: multica daemon status reports running. No daemon restart or reload was performed.
- Residual / owner decision: x99-codex-sol retains task dependencies and raw evidence for review corrections. No human decision is required.
```

These are observed shared-mount changes during the cleanup interval. Concurrent work consumed space on `/tmp`, despite removal of this task's isolated profile. No nominal file-size sum is reported as returned space.

## Second Review Cleanup Receipt

```text
Cleanup receipt
- Mount: workspace; df -B1 before: 306524028928; after: 307595808768; net returned: 1071779840 bytes.
- Mount: /tmp; df -B1 before: 129421426688; after: 129425227776; net returned: 3801088 bytes.
- Removed (exact paths): apps/web/.next; apps/desktop/out; /tmp/che-2123-capture-home.
- Reference check: all local test/build/capture sessions collected; ports 31953 and 31954 returned ECONNREFUSED; git worktree list contains only the task checkout.
- Recoverable because: generated Next/Electron output and the isolated capture profile can be recreated with the committed harness.
- Preserved on purpose: published captures/transcripts, raw/provisional evidence, task dependencies, shared caches, and unrelated reference checkouts.
- Health after: multica daemon status reports running, PID 1915359. No daemon restart or reload was performed.
- Residual / owner decision: x99-codex-sol retains task dependencies and raw evidence for review corrections. No human decision is required.
```

These figures are observed shared-mount free-space changes, not sums of nominal artifact sizes.
