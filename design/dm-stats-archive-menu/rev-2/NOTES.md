## What changed vs rev-1

Answers the UX verdict on `rev-1` at `8a6781e7551c95b87946d21ea18c66781de15cab` (x99-codex-luna-2, comment `01a14347-0c2b-7d5c-99ac-32c56d49b73a`). `rev-1` is untouched and all three directions stay.

- A: "The scope switch hides the requested 'Whole conversation' total rather than showing it beside 'This DM only'." The switch is now a pair of tabs that each carry their own amount ($0.40 and $1.87), so both totals stay visible. The selected tab sets the detail below. Markup uses `role="tablist"`, `role="tab"`, `aria-selected` and `role="tabpanel"`; tabs are 44 px high.
- B: "The paired totals are too compressed on the phone layout." The columns are a real `<table>` with column headers and a caption. Below about 430 px each metric stacks, and every value carries its own "This DM only" or "Whole conversation" label. The first column is bold and the second muted, so the session total leads.
- C: "The phone capture omits the expanded disclosure state." "Show" and "Hide" are a real button with `aria-expanded` and `aria-controls`. Expanded states are captured on web (640 px and 358 px) and on mobile (M2b).
- All: "They omit disconnected." New frames W3d (web) and M2c (mobile) keep the last numbers visible and dimmed, with a time in ICT and Retry.
- All: A11y findings (focus, target size, semantics). New frame W7 shows the visible focus ring, tab order and Esc behavior for the direction. Mobile buttons, tabs and links are at least 44 px high.

## Directions

- A — Scope tabs that carry both totals.
- B — Comparison table, stacked on phones.
- C — Summary first, chain as a disclosure button.

## Limits

- Light theme only. Sample data. Token values copied from `packages/ui/styles/tokens.css`, not the real components.
- The repo still has no `DESIGN.md`, so the consistency finding about product patterns is not resolved here.
- Static HTML. Focus ring and tab order are drawn, not tested. Runtime keyboard behavior is NOT-RUN.
- PNGs are palette-reduced to 128 colors so each stays under the 300 KB image budget. Text and layout are unchanged. Colors can shift slightly, for example the destructive red.
- Rubric scores for `rev-2` come from the different-lab UX reviewer on this exact commit. None are recorded here.
