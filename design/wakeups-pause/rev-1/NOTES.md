## What changed vs rev-0

rev-0 is the live Settings → Wakeups page, which has no pause control. Compared against it:

- Every direction adds a pause control above "When sub-issues finish".
- Every direction shows the per-rule switch as overridden while paused.
- Every direction shows running, paused (admin), paused (member, read-only), in-flight, failed-save and issue-panel states.
- Copy follows the strings shipped on PR #254.

## Directions

- A — Switch card at the top of the tab with a quiet banner under it. This is what PR #254 builds.
- B — Pause is a button. Paused state is a warning banner with a Resume button that replaces the control.
- C — Status pill and switch in the page header. A strip across every page shows the pause to everyone.
