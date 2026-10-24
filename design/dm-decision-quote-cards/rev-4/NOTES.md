## What changed vs rev-3

`rev-1`, `rev-2` and `rev-3` are not edited. This is a content revision: each direction's HTML is the `rev-3` HTML plus two new sections at the end. Everything above them is unchanged, apart from the revision label in the title.

- Forked-DM states (fork lineage A, approved by Cheese on CHE-1945, comment `01a15f27-2c41-7530-8da5-dc5276e6bd05`). Each direction now shows an inherited card in a forked DM: in effect, pending, disclaimed, in a fork of a fork, and a copied bubble that was never quoted (no card).
- Inherited-card controls. The card has no Disclaim. It shows an "Inherited" label and "from the original DM", and clicking it opens the original issue comment. It follows the original decision's state. Disclaim stays in the original DM and on the issue. The server still lets the owner withdraw the original decision (Luna's verdict `01a15ff4-e78a-77f7-8ad1-b34bc2741862`), so hiding the control is a UI rule that PR 2 and PR 3 must implement and test.
- Evidence claims reconciled. See the PR 316 description for the full list. In short: rev-3 said the tiles reassemble with zero pixel difference, and Luna could not confirm that, because the mockup animations made renders differ. rev-4 renders with reduced motion and animations off. Two independent renders of every direction and width are pixel-identical (SHA-256 of the decoded pixels recorded in the PR).
- Live captures redone at the PR 315 head `703ac7170f07e4d5f31a0eb5a0ce158bbd83f0e4` (base `origin/main` `c92ac3ae2b885a9e16875b215b9452f328351bed`), PNG, 1440 px and 390 px, in `rev-4/live/`: issue timeline, DM, and a forked DM (`fork-live-*`, new). The live app has no new UI yet, so the DM still shows the plain-text "Decision recorded" message and the forked DM shows the same message with no card. The yellow "connection is unstable" strip in the DM captures comes from the seeded test agent, not from product UI. The Next.js development badge is hidden.
- Evidence PNGs: each direction at 1440 px and 390 px, rendered from the HTML in this folder, cut into lossless tiles so each file stays under the 300 KB image budget.

## Directions

- A — Quiet quote: unchanged from rev-3, plus the forked-DM states. The inherited card keeps the accent bar.
- B — Receipt card: unchanged from rev-3, plus the forked-DM states. The inherited chip drops its Disclaim.
- C — Timeline row: unchanged from rev-3, plus the forked-DM states. The inherited card keeps the stepper.

## Scores

Not scored yet. The rev-3 scores (Luna, `01a15930-9849-7363-bc59-6c9ff6f92098`) do not carry to rev-4, because the HTML changed. A different-lab UX reviewer must score rev-4 before Cheese sees it.

## Limits

- Static HTML. The flash is shown at its peak frame. Keyboard and screen-reader behaviour is NOT-RUN.
- Live captures show only the current UI. The new cards exist only as mockups.
