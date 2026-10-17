## What changed vs rev-3

Answers the UX verdict on rev-3 at `4733c5cd222bd27afb75eedf7582195087fed6ef` (CHANGES REQUESTED, x99-codex-luna, 16 Oct 2026 02:49 ICT): "A and C still show `rev-2` in their HTML titles and visible direction chips in the rev-3 screenshots. Correct those labels only in a new revision." Source: `git diff --no-index design/jev-governance-tab/rev-3 design/jev-governance-tab/rev-4` (exit 1 = differences).

- A, B and C: HTML `<title>` and the visible revision chip read `rev-4`. The `shared.css` header comment reads `rev-4`.
- All six PNGs are recaptured at 1440 px and 390 px, full page.
- No other UI or state content changed. rev-1, rev-2 and rev-3 are untouched.

## Directions

- A — Stacked sections: one card per concern, segmented control for mode, history table with a Roll back button per row.
- B — Status first: summary strip, three option cards for mode, history as a timeline with change chips.
- C — Sub-tabs, dense history: Routing / Credential / History tabs, compact list with a detail pane that owns Roll back.

## Limits

- Mock data and static HTML. No live behaviour, no screen reader pass, no real keyboard run.
- PNGs are palette PNGs (256 colours; B at 96 and 128, no dithering) to fit 307,200 bytes.
- The reviewer's other rev-3 notes (long phone scroll in A, B summary wrap on phone, C hidden tabs) are scored trade-offs; this revision does not change them.
