## What changed vs rev-1

`rev-1` was published on design PR #321 at commit `4b3058bb6941d2216dec6403709265576dbf41db` and is not edited. It does not land on `main`, because its six PNGs are 316–379 KB and fail the 300 KB image budget. `git diff --no-index <rev-1 folder> design/dm-wakeup-chip/rev-2` over the HTML files shows the changes listed here.

- Phone width, direction A: the instruction line wraps to two lines (line clamp 2) instead of one truncated line. This answers the known limit recorded for rev-1.
- Titles say rev-2. Nothing else changes in the HTML: same three directions, same states, same copy.
- The six PNGs are quantized to a 64-colour palette. They are now 94–119 KB, under the 300 KB budget. No check is bypassed.

## Directions

- A — Strip tucked behind the composer. Same layer as the chat queue. One row per follow-up with Cancel at the row's end. Shows the instruction in a second line, two lines at phone width.
- B — Chips above the composer. One compact chip per follow-up: clock, agent, time and an ✕ that cancels. The chip opens a small dialog with the condition, the instruction, Keep and Cancel follow-up.
- C — Marker in the thread. A dashed marker after the last reply with Cancel on the same line, plus a "N follow-ups" count in the DM header.

No direction is selected or approved. Only Cheese selects, by merging this PR.

## Capture

X99, 20 October 2026. Static HTML, no network. Full-page captures with playwright-core driving the system Google Chrome 150, at 1440 px and 390 px wide, then palette-quantized with Pillow. All six PNGs were checked for content (3.7–14.2% non-light pixels), and three were opened and read. None scrolls sideways. No native `aria-disabled` is left without `disabled`.

Phone widths raise button and chip targets to 40 px, which `DESIGN.md` records as a proposal, not current code.

## Rubric scores

NOT-SCORED. A reviewer from a different lab than the designer (Anthropic) scores each direction of this exact revision before Cheese sees it. Scores for rev-1 do not transfer.
