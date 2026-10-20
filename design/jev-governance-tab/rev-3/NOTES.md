## What changed vs rev-2

Answers the UX verdict on rev-2 at `11afb16ed7bb9f404b7e50a448f224e8feb1bf0e` (CHANGES REQUESTED, x99-codex-luna, 16 Oct 2026 02:10 ICT): "Replace the design note with user-facing copy or remove the design note before Cheese considers B for selection." Source: `git diff --no-index design/jev-governance-tab/rev-2 design/jev-governance-tab/rev-3` (exit 1 = differences).

- B: removed the "design note" box ("Shadow evidence (confident-wrong / verified items) has no read field yet. Static text only until one exists.") from all 7 frames. No replacement copy: the locked On option already states the user-facing reason.
- B: the in-page revision chip reads `rev-3`. B PNGs recaptured (page is about 390 px shorter).
- A and C: HTML, CSS and PNGs are byte-identical copies of rev-2. Their in-page chips still read `rev-2` because the files are unchanged.
- `shared.css` unchanged. rev-1 and rev-2 untouched.

## Directions

- A — Stacked sections: one card per concern, segmented control for mode, history table with a Roll back button per row.
- B — Status first: summary strip, three option cards for mode, history as a timeline with change chips.
- C — Sub-tabs, dense history: Routing / Credential / History tabs, compact list with a detail pane that owns Roll back.

## Limits

- Mock data and static HTML. No live behaviour, no screen reader pass, no real keyboard run.
- B PNGs are 96-colour palette PNGs (no dithering) to fit 307,200 bytes.
- The other rev-2 findings (long phone scroll in A, B summary wrap on phone, C hidden tabs) are design trade-offs the reviewer scored; this revision does not change them.
