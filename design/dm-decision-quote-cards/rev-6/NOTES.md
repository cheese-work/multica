## What changed vs rev-5

`rev-1` to `rev-5` are not edited. rev-6 carries only direction B, which Cheese selected (CHE-1870 comments `01a160f2-1c1d-702e-b019-083e356949f4` and `01a160f2-9379-7720-80c3-8eec261036fe`). It does not reopen A or C.

Source for the findings: Luna's rev-5 UX verdict, comment `01a16132-4c22-787a-8a97-4eb7fbc869f3`.

- Disclaimed issue card renders muted. The disclaimed card's `div` had two `style` attributes, so the browser dropped the second one (the muted background and text colour). It now has one `style` attribute. This defect was already in rev-4 and rev-5. Rendered check in Chrome: the disclaimed card's computed background is `oklch(0.967 0.001 286.375)` and its text colour is `oklch(0.505 0.016 285.938)`. In rev-5 the same card computed to a transparent background and the full-strength text colour. The pending card is unchanged.
- Visible revision heading corrected. The H1 said `rev-4` while the title said `rev-5`. In rev-6 the H1, the title and the lead sentence all say `rev-6`.
- The lead sentence now describes the rev-6 fixes. Nothing else in `B.html` changed (a diff shows two changed lines).
- Evidence PNGs: B at 1440 px and 390 px, rendered with reduced motion and animations off, cut into 2 lossless tiles per width so each file stays under the 300 KB image budget. Two independent renders are pixel-identical.
- The live captures stay in `rev-4/live/`, because the current app has not changed.

## Directions

- B — Receipt card: unchanged in design from rev-5.

## Scores

rev-5 B was scored by Luna (comment `01a16132-4c22-787a-8a97-4eb7fbc869f3`): clarity 9, hierarchy 8, state coverage 8, consistency 6, accessibility 8, density 8. rev-6 is not scored. A fresh different-lab review must cover this revision before the human merge handoff.

## Limits

- Static HTML. Keyboard and screen-reader behaviour is NOT-RUN. The inherited card's read-only rule is a UI rule for PR 2 and PR 3 to build and test.
