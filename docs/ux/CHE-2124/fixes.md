# CHE-2124 F1/F2 verification

Source candidate: `195a1e1eddffe1e87f9027da48e7d40301790cb0`.
Parent before these fixes: `e175d2de915af3fb11ee756a427253d00118f6d9`.
Review request: `01a17c76-ce89-7cf3-9a86-1b96df60a97f`.
Verdict: `01a17c6a-1f82-74e2-a05d-7e980a220126`.

## Scope

- F1: the two existing mode buttons now use `aria-pressed:bg-muted`. The selected background remains after the pointer leaves the control.
- F2: the active change section has a solid 2 px `outline-ring`, offset inward 2 px. The outline paints above the row backgrounds instead of hiding beneath them.
- The PR title ends with exactly one `[squash]` tag. The PR remains draft. Do not merge.
- Non-blocking verdict items 1 to 5 remain unchanged. No locale copy change is included.
- The code-gate PASS on the parent does not carry to the new head. Fresh independent UX/code review remains pending with the coordinator.

## Test-first receipt

On 27 October 2026, the two new component regressions ran before the viewer edit.
At 15:34 ICT, [red-view.log](raw/fixes/red-view.log) exited 1 with 2 failed and 8 skipped:

```text
FAIL PlanRevisionViewer > visually marks only the pressed diff mode
Error: expect(element).toHaveClass("aria-pressed:bg-muted")
FAIL PlanRevisionViewer > moves focus and a visible outline between change sections
Error: expect(element).toHaveClass("outline-2 outline-ring -outline-offset-2")
Received: scroll-mt-8 outline-none ring-1 ring-inset ring-ring/30
```

At 15:35 ICT, the same command exited 0 with [2 passed and 8 skipped](raw/fixes/green-view.log).
These are observed assertion failures, not compile failures or reconstructed red evidence.

From `packages/views`:

```sh
../../node_modules/.bin/vitest run editor/plan-revision-viewer.test.tsx -t 'visually marks only|moves focus and a visible outline'
../../node_modules/.bin/vitest run editor/plan-revision-viewer.test.tsx editor/attachment-preview-modal.test.tsx locales/parity.test.ts
```

The broader command exited 0 with [301 passed](raw/fixes/green-regressions.log).
Views TypeScript and scoped ESLint each exited 0 without diagnostic output; commands are in [checks.txt](raw/fixes/checks.txt).
The [radius gate](raw/fixes/radius.log) passed across 2,115 source files.

## Real-client verification

- [Web](raw/fixes/web-green-rerun.log): 2 passed. Desktop and phone flows include computed backgrounds, actual section focus, a solid 2 px outline, aligned split rows, download and safe literal script rendering.
- [Electron 44.9.3](raw/fixes/desktop-green-rerun.log): 1 passed. The real built client uses isolated HOME/config data, disabled daemon autostart and the locally built fork CLI.
- [Permission probes](fixes/permission-checks.json) reran: discovery, current metadata, current content and base content each returned 200 for the member, 401 without auth, and 404 for foreign-workspace and non-member access.
- The first web attempt timed out during cold Next.js compilation. The rerun passed after compilation finished. The first Electron attempt used the default local API URL; rebuilding with this fixture's API/WS/app URLs fixed the capture harness. Both failed attempts remain in `raw/fixes/`; neither is behavioral TDD red.
- No production browser session or host daemon was borrowed. The fixture still has 360 lines and three-digit line numbers.

## UI review pack

Preview: not available after task cleanup.
Design: `design/plan-revision-viewer/rev-2/A.html` @ `aa8306b19818658407f6eefcc495859b026b5a8a`.
Sheet: `../CHE-2124.yml`.
Visual diff: NOT-RUN. Screen-reader review: NOT-RUN.

| Page | Width | Before these fixes | Fresh captures in `fixes/` |
| --- | --- | --- | --- |
| Web attachment preview | 1440 | `web-1440-unify.png`, `web-1440-split.png` at `8ad850eb95a98ac2a7c78fa27cee356e3b66ac30` | `web-1440-off.png`, `web-1440-unify.png`, `web-1440-split.png`, `web-1440-next.png`, `web-1440-previous.png`, `web-1440-base.png` |
| Web attachment preview | 390 | `web-390-unify.png` at the same original implementation | `web-390-off.png`, `web-390-unify.png`, `web-390-next.png`, `web-390-base.png` |
| Real Electron | 1440 | `desktop-1440-unify.png`, `desktop-1440-split.png` at the same original implementation | `desktop-1440-off.png`, `desktop-1440-unify.png`, `desktop-1440-split.png`, `desktop-1440-next.png`, `desktop-1440-previous.png`, `desktop-1440-base.png` |

The original pre-feature web baseline remains in the parent directory.
Pre-feature desktop before remains NOT-CAPTURED; the original Electron implementation captures are a baseline for these two fixes only.
Fresh screenshots are real product images, not mockups. The web images include Next.js development chrome.

Manual Keep / Change / Remove:

- Keep: current Markdown with Diff off, download, base picker and original optional actions. No actions are removed.
- Change: the pressed Split/Unify background is visible without hover on web and Electron.
- Change: next/previous moves actual focus and the opaque outline between the correct named change sections on web and Electron.
- Phone observation: next-change navigation can scroll the toolbar above the viewport, as shown in `web-390-next.png`. The existing `scrollIntoView` behavior is unchanged by this styling-only fix. The initial phone toolbar and overflow remain in the other phone captures.
- Removed: none. The author does not treat this pack as an independent UX PASS.

[Cleanup receipt](fixes-cleanup.md): task capture services, fixture database and generated builds/dependencies were removed. Shared services remain healthy.
