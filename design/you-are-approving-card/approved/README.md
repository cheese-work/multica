Locked from doop page `MJ4yJPTHt6:page1` (file https://congvc-c00.taila6fa8a.ts.net:34443/c/MJ4yJPTHt6), frame `t8_q2WrMLL`, `updatedAt` `2026-10-30T12:12:16.518Z`. Layer 2 of [CHE-2606](mention://issue/01a18c1e-b128-7bf8-a827-17c4188b8258); design issue [CHE-2611](mention://issue/01a18c21-9537-7a08-a4fa-684d6dafc545), lock issue [CHE-2656](mention://issue/01a18d85-632d-7451-853f-b2d8173ecd3f).
Direction B (tabbed viewer) selected by Cheese (`01a18d7e-2504-73ce-9bba-a1cf0ef5c649`, answer to request `dd0ea7df-5169-4b0f-8bde-7db62a76939c`), UX PASS on this exact version (`01a18cc3-3989-77c8-b495-e8bd0114dfc0`, x99-codex-luna-design).
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

- `approving-card-b.html`: the frame as read from doop, unchanged (system font stack, inline SVG, no external fetch). Its hash is `screens[0].content_sha256` in `LOCK.json`.
- Shows, desktop 540 and phone 358, light and dark: collapsed (default), changes open, preview open, loading changes, changes failed to load, first revision (Preview tab only), no artifacts (renders as today), answered (record of what was approved). The phone column shows states 1, 2, 3 and 5.

## Implementation must add and test

The UX reviewer scored B 6/10 on accessibility. The mockup does not show these; they are build items, each needs a test:

- Tab and panel linkage: each `role="tab"` has `aria-controls` pointing to its `role="tabpanel"`, and the panel has `aria-labelledby` pointing back. The mockup tabs carry no `id` or `aria-controls`.
- Arrow-key movement: Left and Right (and Home and End) move focus between tabs; only the selected tab is in the tab order.
- Selecting the open tab again closes the viewer, as the frame notes describe.
- The diff and preview scroll regions stay focusable (`tabindex="0"`) with labels, as in the frame.
