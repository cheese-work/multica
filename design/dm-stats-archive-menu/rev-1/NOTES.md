## What changed vs rev-0

- No earlier revision. The baseline is the current code, not captured live screens: the repo has no `DESIGN.md`, and no running web app was captured for this pass.
- New surfaces: Statistics dialog and sheet with the DES-2 order, one more-menu order (DES-5), slash suggestions with a "runs the agent" hint, the A2 unarchive-on-send dialog, an Archived list with no pins.
- The three directions differ only in how the "Whole conversation" total sits beside "This DM only". Every other frame is identical across A, B and C.
- Stats copy reads from DES-2 and DES-8. "Usage missing for 2 finished runs" and "—" stand for partial and unavailable cost.

## Directions

- A — Scope switch: a segmented control chooses one total at a time; hidden when the DM has no chain.
- B — Side-by-side columns: "This DM only" and "Whole conversation" columns in every summary row.
- C — Summary first, chain as a disclosure: own numbers lead, one row expands to the whole-conversation total.

## Limits

- Light theme only. Dark theme, motion and real focus order are not shown.
- Static HTML with sample numbers. Layout uses token values copied from `packages/ui/styles/tokens.css`, not the real components.
- Desktop PNGs are 1440 px wide, phone PNGs 390 px wide, both full page. The mobile-app frames are drawn at 360 px inside both.
- Rubric scores come from the different-lab UX reviewer on the exact commit. None are recorded here.
