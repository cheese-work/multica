## What changed vs rev-6

`rev-1` to `rev-6` are not edited. rev-7 carries only direction B, which Cheese selected (CHE-1870 comments `01a160f2-1c1d-702e-b019-083e356949f4` and `01a160f2-9379-7720-80c3-8eec261036fe`). It does not reopen A or C.

Source for the finding: Luna's rev-6 UX verdict, comment `01a16162-6394-7a98-b8a5-77dca41b6142`. Conflict states were missing. The only cue was a generic retry error.

- New section "Disclaim conflicts": the Disclaim endpoint returns two different 409 answers, and rev-7 shows both on the issue card and on the DM chip.
  - "Decision context changed" (the server says `decision context changed; retry`): a warning notice under the card says "This decision changed while you were reading. Refresh to see the current state before you disclaim. Nothing was withdrawn." The action is **Refresh**. Disclaim stays disabled until the person refreshes. There is no blind retry.
  - "Already withdrawn" (the server says `decision answer is already withdrawn`): the card turns muted with the Disclaimed pill and loses Disclaim. A notice says "Already withdrawn. Nothing was changed by this click." The action is **Review**, which opens the withdrawal comment (issue) or the issue comment (DM).
- No reason input. The confirm dialog is unchanged, and it closes before the 409 notice appears.
- Drafts are preserved. Each example shows a reply draft (issue) or a message draft (DM) marked "kept" next to the notice.
- The generic "Could not disclaim" alert with Try again stays for other errors, and the earlier error state is unchanged.
- The H1, the title and the lead sentence say `rev-7`. Everything above the new section is the rev-6 B file (a diff shows only the revision labels and the lead sentence).
- Evidence PNGs: B at 1440 px and 390 px, rendered with reduced motion and animations off, cut into 3 lossless tiles per width so each file stays under the 300 KB image budget. Two independent renders are pixel-identical.
- The live captures stay in `rev-4/live/`, because the current app has not changed.

## Directions

- B — Receipt card: unchanged in design from rev-6, plus the conflict states.

## Scores

rev-6 B was scored by Luna (comment `01a16162-6394-7a98-b8a5-77dca41b6142`): clarity 9, hierarchy 8, state coverage 8, consistency 9, accessibility 8, density 8. rev-7 is not scored. A fresh different-lab review must cover this revision before the human merge handoff.

## Limits

- Static HTML. The 409 behaviour is shown, not run. Navigation, Refresh and Review actions, permission enforcement, keyboard and screen-reader behaviour are NOT-RUN. The read-only inherited card is a UI rule for PR 2 and PR 3 to build and test.
