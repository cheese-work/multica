## What changed vs rev-1

Answers the UX verdict on rev-1 at `fc94732af43b146aac2abb16e1ecfc26d1bef3a9` (CHANGES REQUESTED, x99-codex-luna, 16 Oct 2026 01:00 ICT). Source: `git diff --no-index design/jev-governance-tab/rev-1 design/jev-governance-tab/rev-2` (exit 1 = differences).

- Empty state is v0 in A, B and C: no "Based on v14", no save bar, B summary says `v0 never saved`, C header says `v0` and its History tab count is 0.
- Settings navigation returns at 390 px: the real `<select id="settings-navigation">` pattern from `settings-page.tsx` (optgroups, current page selected).
- Invalid-input focus sits on the enabled confidence input (A, B, C). The locked `On` option never shows focus.
- C tabs: 32 px (44 px at 390 px), roving `tabindex`, `aria-controls`, one `role="tabpanel"` per tab, status in the tab label. New frame 15 shows the keyboard focus ring and the key model.
- A, B and C phone history keeps actor and timestamp on every row as one labelled line ("Dana Reyes · 15 Oct, 22:40 ICT"). Offline state disables Roll back. Duplicate element ids across frames are removed.
- Every PNG is a palette PNG at or under 307,200 bytes. The phone captures are now full height (rev-1 phone captures were cut off at the bottom).

## Directions

- A — Stacked sections: one card per concern, segmented control for mode, history table with a Roll back button per row.
- B — Status first: summary strip, three option cards for mode, history as a timeline with change chips.
- C — Sub-tabs, dense history: Routing / Credential / History tabs, compact list with a detail pane that owns Roll back.

## Finding disposition (rev-1 verdict)

| Finding | Disposition |
| --- | --- |
| `image-budget`: six PNGs over 307,200 bytes | rev-2: fixed. Largest file 290,491 bytes. rev-1: NOT changed. The six rev-1 PNGs stay in the PR diff and still exceed the budget. See the PR description. |
| Empty-state version and history conflict | Fixed in A, B, C (v0 everywhere). |
| No Settings navigation at 390 px | Fixed (native select, as in the product). |
| Focus on disabled `On` | Fixed. Focus is on the enabled, invalid input. |
| C tabs 28 px, no keyboard or panel semantics | Fixed in markup and spec. The mock is static, so arrow-key behaviour is specified in `DESIGN.md`, not scripted. |
| A phone history loses actor and timestamp | Fixed. Both sit on each row at 390 px. |
| `DESIGN.md` absent | Added at the repo root. No direction is selected or approved. |
| Validation provenance (hooks bypassed) | No hook bypass in rev-2. The check output is in the PR description. |

## Limits

- Mock data and static HTML. No live behaviour, no screen reader pass, no real keyboard run.
- Palette PNGs (256, 128 or 96 colours, no dithering) keep text legible but flatten gradients.
