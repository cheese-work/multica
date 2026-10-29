## What changed vs rev-4

`rev-1` to `rev-4` are not edited. rev-5 carries only the direction Cheese selected, B (Receipt card). It does not reopen A or C.

- Selection: Cheese chose B on CHE-1870, comment `01a160f2-1c1d-702e-b019-083e356949f4` ("I choose B, receive card"), corrected by comment `01a160f2-9379-7720-80c3-8eec261036fe` ("Correction: receipt card"). The native request `f583b643-7097-43a9-a2f8-087a791f4916` still reads `open`, so this record cites the issue comments and not a DM record. A chat pick is a selection and not the approval.
- One change to B: the inherited card on a forked DM now shows the visible text "from the original DM" after "Quoted in CHE-1870". The rev-4 UX verdict named this as B's biggest problem (comment `01a1603d-6c50-7c36-a806-f5a07d151c79`). Four inherited chips changed. Everything else in `B.html` is the same as rev-4, apart from the revision label and the lead sentence.
- The live captures stay in `rev-4/live/`. rev-5 adds none, because the current app has not changed.
- Evidence PNGs: B at 1440 px and 390 px, rendered from `B.html` with reduced motion and animations off, cut into 2 lossless tiles per width so each file stays under the 300 KB image budget. Two independent renders are pixel-identical.

## Directions

- B — Receipt card: the quote sits in a bordered decision card with a header strip for class and state, and the actions inside the card. The inherited chip says "Quoted in CHE-1870 · from the original DM", and carries an "Inherited" badge and no Disclaim.

## Scores

rev-4 B was scored by Luna (comment `01a1603d-6c50-7c36-a806-f5a07d151c79`): 9, 9, 9, 8, 7, 8. rev-5 is not scored. A fresh different-lab review must cover the changed HTML before the human merge handoff.

## Limits

- Static HTML. Keyboard and screen-reader behaviour is NOT-RUN. The inherited card's read-only rule is a UI rule for PR 2 and PR 3 to build and test.
