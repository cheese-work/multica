# CHE-2124 verification

F1/F2 follow-up: [fixes.md](fixes.md), with fresh real web/Electron captures at `195a1e1eddffe1e87f9027da48e7d40301790cb0`.
The evidence below records the original implementation and radius correction, not the new review candidate.

Implementation: `8ad850eb95a98ac2a7c78fa27cee356e3b66ac30`.
Baseline: `84afc806a3e4bf8ae474f788defafa3afaf68a43` from a detached checkout.
Design: `design/plan-revision-viewer/rev-2/A.html` at `aa8306b19818658407f6eefcc495859b026b5a8a`.

The final candidate also replaces the Readonly badge's bare `rounded` with `rounded-sm`.
The radius checker caught the bare utility in CI and in the local reproduction before the fix.
The named small token resolves to 4 px with the current 8 px base.
Captures below remain from the implementation commit, before that token-only correction.
The complete frontend suite and captures were not repeated after that correction.

The PR stays draft. Independent UX and code verdicts are pending.
Cheese merges the code PR. No deployment is included.

## Revision linking

Only exact `plan-rev-N.md` names participate. `N` is a positive safe integer without leading zeroes.
The current attachment's source issue supplies the revision list.
Candidates must belong to that same issue and workspace, with a smaller revision number.
The greatest smaller number is the default base, including numeric gaps.
Duplicate uploads at that number require an explicit attachment-ID choice.
Labels include filename, ICT time, uploader and immutable ID.
Comment metadata and `--supersedes` are not required.
CHE-2125 and CHE-2121 were read, not edited.

## Observed test-first evidence

All recorded times are on 27 October 2026, ICT.

| Check | Observed red | Observed green |
| --- | --- | --- |
| Lookup and literal diff | `raw/red-core.log`: 6 failed; expected revision 12, received null | `raw/green-core.log`: 6 passed at 13:58 ICT |
| Viewer wiring | `raw/red-view.log`: 5 failed; current document and controls absent | `raw/green-view.log`: 299 passed at 14:05 ICT, including 8 viewer regressions, existing previews and locale parity |
| Bounded line count | `raw/red-line-limit.log`: 1 failed; oversized identical input did not throw | Same core command: 6 passed |
| Overflow menu | `raw/red-menu.log`: live phone flow failed because a menu label lacked its group context | `raw/green-web.log`: live flow passed after the context fix |
| Radius quality gate | `raw/red-radius.log`: one bare `rounded` utility at the Readonly badge | `raw/green-radius.log`: 2,115 source files clean; `raw/green-radius-tests.log`: 4 passed |

The red outputs are behavioral assertion failures, not collection or compile failures.
Initial core red ran before helper implementation; initial viewer red ran against the minimal viewer stub.
Additional pending/error/recovery tests are regression coverage, not reconstructed red evidence.

Commands, from the indicated package:

```sh
cd packages/core
../../node_modules/.bin/vitest run attachments/plan-revisions.test.ts
cd ../views
../../node_modules/.bin/vitest run editor/plan-revision-viewer.test.tsx editor/attachment-preview-modal.test.tsx locales/parity.test.ts
```

## Live verification

- Web: 2 passed. The desktop and baseline capture cases are deliberately separate opt-in runs.
- Real Electron 44.9.3: 1 passed, using the built desktop client on X99 with headless Ozone.
- Baseline web capture: 1 passed, using the detached pre-change checkout.
- TypeScript: core, views, web and desktop renderer passed. Scoped views ESLint passed.
- Electron production renderer/main/preload build passed. An existing mixed static/dynamic import warning remains in the emoji picker.
- Permission checks: source-issue attachment list, current metadata, current content and base content each returned 200 for the member, 401 without auth, and 404 for foreign workspace and non-member access. See `permission-checks.json`.

The live fixture has 360 lines. The phone capture includes three-digit line numbers.
The web test checks change navigation, literal script source without execution, aligned split row heights, phone Unify default, overflow controls and the downloaded filename.
The desktop run uses a separate HOME and profile. Daemon autostart is disabled.
Only a locally built fork CLI is bundled. No production browser session or host daemon is borrowed.

The diff bounds total source lines at 20,000 and the LCS grid at 4,000,000 cells.
An oversized comparison falls back to the current document with a visible error and the existing download action.
Existing attachment-content size and authentication limits are unchanged.

## UI review pack

Preview: not available. Local capture services are task-owned and stop before handoff.
Sheet: `docs/ux/CHE-2124.yml`.
Visual diff: NOT-RUN; CHE-1485 Layer B automation is deferred.

| Page | Width | Before | After |
| --- | --- | --- | --- |
| Web attachment preview | 1440 | `web-1440-before.png` | `web-1440-off.png`, `web-1440-unify.png`, `web-1440-split.png`, `web-1440-base.png` |
| Web attachment preview | 390 | `web-390-before.png` | `web-390-off.png`, `web-390-unify.png`, `web-390-base.png` |

Real desktop after captures at 1440 px: `desktop-1440-off.png`, `desktop-1440-unify.png`, `desktop-1440-split.png`, `desktop-1440-base.png`.
The before captures are web captures of the former shared reader, not desktop before captures.

Manual Keep / Change / Remove check:

- Keep: `revision-document` with Diff off; Download in the toolbar or phone menu; Close when the modal supplies it. Callback behavior is covered by component tests.
- Change: compact one-row toolbar; phone Diff/count/arrows remain visible; secondary actions share one menu. Matches the recorded phone decision.
- Change: authenticated same-issue bases; collapsed unchanged context; previous/next controls; literal Unify and aligned Split. Matches the sheet.
- Removed: none. Other attachment types retain the existing viewer. Source location, file info, overview and new-tab actions remain available when supplied.

All PNGs are real product captures. The design mockup is not used as implementation evidence.
Mermaid syntax and browser rendering passed; GitHub's rendered diagram is NOT-RUN until observed.
Independent UX scoring and the final different-lab code review remain with the coordinator.
