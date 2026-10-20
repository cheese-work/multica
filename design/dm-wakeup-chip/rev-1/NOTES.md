## What changed vs the current live screens

This is the first revision. The live DM window has no affordance for a scheduled follow-up today. The closest pattern is the chat queue strip tucked behind the composer (`packages/views/chat/components/chat-queue.tsx`).

- Adds a place to see each pending follow-up and cancel it, in three placements.
- Every direction shows the states `DESIGN.md` requires: loading, error with Retry, disconnected, and the empty state (nothing rendered).
- Adds the states this feature needs: one follow-up, a follow-up with a condition and a deadline, three follow-ups (the cap), inherited from the previous DM, cancelling, cancelled, cancel failed, and the moment the turn starts.
- Copy keeps Cheese's wording, "<agent> checks back 11:40 ICT", and adds "when CHE-1812 is done, or by 11:40 ICT" for a condition. Times are ICT.

## Directions

- A — Strip tucked behind the composer. Same layer as the chat queue. One row per follow-up with Cancel at the row's end. Shows the instruction in a second line.
- B — Chips above the composer. One compact chip per follow-up: clock, agent, time and an ✕ that cancels. The chip opens a small dialog with the condition, the instruction, Keep and Cancel follow-up.
- C — Marker in the thread. A dashed marker after the last reply with Cancel on the same line, plus a "N follow-ups" count in the DM header.

No direction is selected or approved. Only Cheese selects, by merging this PR.

## Capture

X99, 20 October 2026. Static HTML, no network. PNGs are full-page captures made with playwright-core driving the system Google Chrome 150, at 1440 px and 390 px wide. All six PNGs were opened and checked: none is blank, and none scrolls sideways. No native `aria-disabled` is left without `disabled`.

Phone widths raise button and chip targets to 40 px, which `DESIGN.md` records as a proposal, not current code.

## Rubric scores

NOT-SCORED. A reviewer from a different lab than the designer (Anthropic) scores each direction before Cheese sees it.
