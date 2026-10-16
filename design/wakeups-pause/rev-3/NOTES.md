## What changed vs rev-2

`git diff --no-index design/wakeups-pause/rev-2 design/wakeups-pause/rev-3` shows every direction rebuilt. rev-1 and rev-2 are untouched.

Answers to the independent UX verdict on rev-2 (x99-codex-luna-2, gpt-6-luna, OpenAI; advisory, not approval) and to `DESIGN.md` required states:

- State coverage, all directions: "no saved or data-load-error view". Added Saved, Resumed, Loading the workspace settings and Could not load the workspace settings (with Retry) states.
- Accessibility, A and C: "the member switch has `aria-disabled` without native `disabled`". Every disabled switch and button now carries native `disabled`. The capture script found no `aria-disabled` left without it.
- Saving and resuming controls are also natively disabled.

Unchanged from rev-2: copy, warning tone, header chip text, the B phone layout.

## Directions

- A — Switch card with the warning banner. This is the layout in the CHE-1727 description and what PR #254 builds.
- B — The same switch while running. While paused, a warning banner with a Resume button.
- C — Switch in the page header. While paused, a strip across every page for everyone.

No direction is selected or approved. Only Cheese selects, by merging this PR.
