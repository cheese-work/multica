## What changed vs rev-1

`git diff --no-index design/wakeups-pause/rev-1 design/wakeups-pause/rev-2` shows every direction rebuilt. rev-1 is untouched.

Answers to the independent UX verdict on rev-1 (x99-codex-luna-2, gpt-6-luna, OpenAI; advisory, not approval):

- Task clarity, all directions: "does not explain one-shot and condition behavior after resume". The description now says due one-shots and conditions run once after resume.
- A hierarchy: "the quiet banner understates a workspace-wide stop". The paused banner uses the warning tone.
- A state coverage: "No resuming state appears". Resuming and pausing states are added to every direction.
- B consistency and density: "A button replaces the requested switch" and "Pause/Resume labels wrap at 390 px". The running state is now the same switch as A. The Resume button sits under the text on a phone. The red Pause button is gone.
- C state coverage and consistency: "the member example omits the promised global strip" and "the page-level pill duplicates it". The strip shows to members. The pill is removed.
- Accessibility, all directions: keyboard focus was unverified. Interactive elements now carry a visible `:focus-visible` outline. Computed contrast: warning text on the warning background 10.5:1, error text on the error background 5.9:1 (it was 4.3:1), muted text on surface 5.9:1.
- Density, phone: the sub-issue block now appears only in the running and paused states, not in every state.
- Header chip: the issue header shows "Wakeups paused" instead of the next due time (the code review finding on PR #254).

## Directions

- A — Switch card with the warning banner. This is the layout in the CHE-1727 description and what PR #254 builds.
- B — The same switch while running. While paused, a warning banner with a Resume button.
- C — Switch in the page header. While paused, a strip across every page for everyone.

No direction is selected or approved. Only Cheese selects, by merging this PR.
