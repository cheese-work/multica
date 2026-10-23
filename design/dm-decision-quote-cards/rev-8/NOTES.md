## What changed vs rev-7

`rev-1` to `rev-7` are not edited. rev-8 carries only direction B, which Cheese selected (CHE-1870 comments `01a160f2-1c1d-702e-b019-083e356949f4` and `01a160f2-9379-7720-80c3-8eec261036fe`). It does not reopen A or C.

Source for the finding: Luna's rev-7 UX verdict, comment `01a16191-f63a-7e2a-b551-e1c7940e1eca`. The conflict callouts used literal OKLCH colours, and `DESIGN.md` says mockups use semantic tokens.

- The "decision context changed" callout, on the issue card and on the DM chip, now uses repository semantic tokens from `packages/ui/styles/tokens.css` instead of raw OKLCH values:
  - background: `color-mix(in oklab, var(--warning) 10%, transparent)` (the code uses `bg-warning/10` for the same tone)
  - border: `1px solid color-mix(in oklab, var(--warning) 30%, transparent)` (`border-warning/30`)
  - text: `var(--foreground)`
  The mockup `:root` block already copies `--warning` and `--foreground` from the token file.
- Nothing else changed. A computed-style comparison of all 648 elements between rev-7 and rev-8 in Chrome finds exactly the two callouts and their text spans changed. Two `.flash` elements also differ in that dump only because their animation was sampled mid-flight. Under reduced motion they render identically.
- The warning tone is a little different from rev-7: the raw colours were a tinted yellow and brown, and the token mix gives a lighter tint with the normal text colour. This is the only visible change, and it is the point of the finding.
- Other literals that rev-7 inherited from earlier revisions, for example the `.pill.pend` and `.err` styles, are not touched, because the ask was limited to the conflict callouts. They appear in `rev-1` to `rev-7` as posted.
- The H1, the title and the lead sentence say `rev-8`. The 1440 px page is 20 px shorter because the lead sentence is shorter (one line fewer). The 390 px page has the same height.
- Evidence PNGs: B at 1440 px and 390 px, rendered with reduced motion and animations off, cut into 3 lossless tiles per width so each file stays under the 300 KB image budget. Two independent renders are pixel-identical.
- The live captures stay in `rev-4/live/`, because the current app has not changed.

## Directions

- B — Receipt card: unchanged in design from rev-7, with token-only conflict callouts.

## Scores

rev-7 B was scored by Luna (comment `01a16191-f63a-7e2a-b551-e1c7940e1eca`): clarity 9, hierarchy 8, state coverage 10, consistency 8, accessibility 8, density 7. rev-8 is not scored. A fresh different-lab review must cover this revision before the human merge handoff.

## Limits

- Static HTML. The 409 behaviour is shown, not run. Navigation, Refresh and Review actions, permission enforcement, keyboard and screen-reader behaviour, and measured contrast are NOT-RUN. The contrast of the new callout text on the new tint is not measured.
