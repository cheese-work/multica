Locked from doop page `aBJFsygiKT:page1` (file https://congvc-c00.taila6fa8a.ts.net:34443/c/aBJFsygiKT), frame `Z3bizs5RyO`, `updatedAt` `2026-11-11T23:21:50.584Z`. Issue [CHE-5162](mention://issue/01a1c949-4c66-744f-a7e4-8a45dcaa594a).
Direction B (one composer, three modes) selected by Cheese (decision request `aa80db43-66a3-4f3b-8782-e13672508011`, answer comment `01a1cce0-7263-747f-ba85-d09e4ac44ae4`, recorded as comment `01a1cceb-89b3-7a02-ad8c-ad2530a73801`) with the note that the Approve button carries a passkey icon. UX PASS on this exact version (`01a1ccf1-6439-7265-a6a8-8ea6e1a5a7f3`, x99-codex-luna-design).
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

- `direction-b.html`: the frame as read from doop, unchanged. Its hash is `screens[0].content_sha256` in `LOCK.json`.
- Shows: one composer with an Approve / Reject / Reply mode switch (Approve preselected); the one primary button names the mode and Enter presses it; Approve starts the passkey step and saves a typed note; Reply says "does not approve" and wakes the agent. Plain-language ask, "if you do nothing" line, before → after rows, JSON behind Details, one batch card per goal, a config-change card led by a server-computed diff, applied / not-applied outcomes, AgentX CLI shape and DevX action template with its failing test.

## Mode rule (Q = B)

Only Approve mode may start a passkey. A typed word is never parsed. Reject and Reply never run the action. The passkey tap is always required.

## Implementation must add and test

- Mode switch as a radio group or tabs with arrow-key movement and `aria-checked`; Enter in the note box presses the primary button of the selected mode. The mockup spans have `tabindex` only.
- Final passkey glyph from the product icon set (`packages/ui/styles` icons, see `AGENTS.md` icon rule). The mockup glyph is a drawn stand-in, not the FIDO mark.
- Token alignment: the mockup uses raw colours; the code slice maps them to semantic tokens.
- Status is an icon and a word, never colour alone.
- All copy needs every supported translation.

## Not covered by this lock

- Directions A and C are not locked.
- No live keyboard, passkey or assistive-technology flow was tested (static HTML).
- Light theme, loading, empty and error states are not mocked.
