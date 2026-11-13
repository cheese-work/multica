Locked from doop page `mRAnnjvGod:page1` (file https://congvc-c00.taila6fa8a.ts.net:34443/c/mRAnnjvGod), frame `UVi0hFp3L_`, `updatedAt` `2026-11-05T10:56:09.927Z`. Issue [CHE-3986](mention://issue/01a1ab20-e4bd-72e0-b96f-a0d635014d04).
Direction A (summary row) selected by Cheese (decision request `5ef6ed71-e1af-4a2e-98b2-4d500f30552d`, recorded as comment `01a1ab4e-d139-78ee-a909-d1cb43492c25`, note "C but adapt A"), reading built as v2 and approved by Cheese in the issue run ("approve v2"). UX PASS on this exact version (`01a1ab5b-be84-74f7-9cfe-5098833dfa85`, x99-agy-gemini-ux).
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

- `issue-sidebar-compact-v2.html`: the frame as read from doop, unchanged. Its hash is `screens[0].content_sha256` in `LOCK.json`.
- Shows: issue sidebar with Pull requests above Wakeups; one `Defaults · N on` button in the Wakeups header; the button opens an inline panel (no popover, no sheet, no focus trap) with the default rules, each with a one-line row and a more menu (Override, Reset to default, Edit, Open project); warm `Defaults · N paused` state with icon and word; empty live list with the line "No live wakeups. Defaults still apply."; `N edited` suffix; desktop 320 px column and 390 px with 40 px targets.

## Reading of the pick

Cheese's note "C but adapt A" is built as C's placement (header button, closed height about 234 px) with A's mechanics (opens inline, rows keep their menu). Override, edit, reset, edge rules and shared states are the A and Shared frames of the proposal page, unchanged.

## Implementation must add and test

- The header button is a real `button` with `aria-expanded` and `aria-controls`; Esc closes the panel and returns focus to the button.
- Paused and edited states use an icon and a word, never colour alone.
- Override takes three clicks from the closed state (open, more, Override). The mockup accepts this; do not add a second entry point without a new design.
- 390 px targets (40 px) and the heights in the mockup are proposals computed from row sizes, not measured in a browser. The 390 px live capture was not taken (X99 has no signed-in session).
