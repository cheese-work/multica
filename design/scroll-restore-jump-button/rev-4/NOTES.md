## What changed vs rev-3

- Feedback (x99-codex-luna, signed 20 October 2026 04:36 ICT, comment `01a1561b-ed66-795b-8df6-39b27e23bc12`): "the busy jump button lacks native `disabled`." Every busy jump button (3 per direction: desktop, web phone, native phone) now has native `disabled`, `aria-busy="true"` and `aria-describedby`. `aria-disabled` is removed because `disabled` replaces it.
- Feedback: the visible reason "does not explicitly explain the jump-button disabled state." The strip text now reads "Retry and Jump to latest messages are off until this attempt ends." The button points at it with `aria-describedby`. The load-retry block has no jump button and keeps "Retry is off until this attempt ends."
- Focus styling: `.jb.focus` stays for the enabled-state focus examples. A real `.jb:focus-visible` rule uses the same `--foreground` outline. `.jb:disabled` gets `cursor: not-allowed` and no focus ring, because a disabled control cannot hold focus (`DESIGN.md`).
- Retry sizes are unchanged: 36 px on desktop, 44 px on phone and native.
- Nothing else in the three HTML files changed. `rev-1`, `rev-2` and `rev-3` are byte-identical to the posted files.

## Directions

- A — Neutral raised disc: surface-raised fill, `--muted-foreground` ring, menu shadow.
- B — Brand-filled disc with floating shadow and a canvas ring. Competes with the composer send button.
- C — Translucent blurred halo around an opaque icon backing, muted arrow, `--muted-foreground` ring, no shadow. Lowest emphasis.

## Static disabled, tab and activation check

Command: `disabled-check.js` is injected at the end of `<body>` in a temp copy of each direction. Headless Chrome 150 runs it with `--dump-dom`. Results are in `disabled-check.json`.

| Check | A | B | C |
| --- | --- | --- | --- |
| Busy jump buttons found | 3 | 3 | 3 |
| Each has `disabled` and matches `:disabled` | yes | yes | yes |
| `focus()` leaves focus on the button | no, 0 of 3 | no, 0 of 3 | no, 0 of 3 |
| `click()` delivers a click event | no, 0 of 3 | no, 0 of 3 | no, 0 of 3 |
| In the frame's tabbable set | no, 0 of 3 | no, 0 of 3 | no, 0 of 3 |
| `aria-describedby` resolves to the strip text | yes | yes | yes |
| Disabled Retry buttons: same three results | 6 of 6 | 6 of 6 | 6 of 6 |
| Enabled jump button takes focus and shows the 2 px `--foreground` outline | yes | yes | yes |
| Enabled-state focus example frames kept | 1 | 1 | 1 |

Limits: the check reads DOM properties in one browser. It does not press Tab, so it does not prove real tab traversal. It does not test a screen reader reading the description. It tests static HTML, not the React web build, and not React Native, where `disabled` on a pressable behaves its own way. The build must repeat the check on the real component.

## Contrast (carried from rev-3)

The enabled button styles are unchanged, so `../rev-3/contrast-check.html`, `.png` and `.json` still describe them. A disabled control is exempt from WCAG contrast, and the busy button keeps the same colours.

- The 18 samples cover three flat underlays: page canvas, assistant bubble and user bubble. They do not cover arbitrary content, product render or native render.
- B's dark icon is 3.11:1. The selected-pixel method takes the best arrow pixel against one backing pixel. It is not a worst-case glyph measurement.
- None of this is a product or native PASS.

## Image budget

- Policy: `scripts/check-image-budget.mjs` fails a PR that adds a bitmap over 300 KB (`IMAGE_BUDGET_BYTES = 300 * 1024`) unless the PR description carries an `Oversized image exemption:` line. The workflow passes the current PR body as `PR_BODY` (`.github/workflows/ci.yml`, job `image-budget`).
- The rev-4 direction PNGs are 480 to 646 KB each, so all six are over 300 KB. A pass on this head means the existing exemption line in the PR description applied. It does not mean the bitmaps are under budget.
- No change to the budget script, no skipped check, no new waiver. The exemption line in the PR description is the existing one, unchanged.
- No earlier capture was recompressed or deleted. The rev-4 direction PNGs are palette-reduced to 256 colours, as in rev-3, so the upload fits.
- The CI result for this head is in the CHE-1857 handoff comment.

## Dispositions carried from rev-3 (unchanged)

- Read-only: one shared pattern, drawn for agent archived and access revoked on web and archived chat on native. Session archived is the same pattern with its own placeholder, not drawn separately. See `rev-3/NOTES.md`.
- Conflict (409 `chat_compacting`): web only. Native has no compacting state in current code, so no native frame. No native behavior is invented.
- Placement: bottom center, fixed. D1 = C is the anchor behavior, not visual direction C.

## Baseline capture: NOT-CAPTURED

Unchanged. No session created, no credential moved, no MBP capture. Decision CHE-1857-BASELINE-HOST-1 stays pending.

## Verification limits

- Static HTML from copied tokens. Not rendered by React, Virtuoso, FlashList or React Native. Fonts are the browser system font.
- Native frames reuse the web token values for dark mode and draw the status bar and home indicator as simple shapes.
- Retrying copy and disabled-reason copy are new placeholders.
- Keyboard behavior, scroll motion and reduced-motion behavior are described in the page text and not drawn.
- `visual diff: NOT-RUN`.
