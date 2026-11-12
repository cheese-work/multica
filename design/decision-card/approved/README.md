Locked from doop page `aBJFsygiKT:page1` (file https://congvc-c00.taila6fa8a.ts.net:34443/c/aBJFsygiKT), frame `Z3bizs5RyO`, `updatedAt` `2026-11-12T01:38:19.680Z` (12 Nov 2026 08:38 ICT). Issue [CHE-5162](mention://issue/01a1c949-4c66-744f-a7e4-8a45dcaa594a).
Direction B v4 (two choices, one composer, one button that names the result). Cheese chose B, then revised the composer model on 12 Nov 2026 08:33 ICT (comment `01a1cd63-7807-7887-ae08-22e9e410cf81`, relayed in `01a1cd64-81d1-76d2-927e-f948099036e3`). This supersedes the 06:10 ICT pick (`01a1cce0-7263-747f-ba85-d09e4ac44ae4`), made from the earlier three-mode frame. UX PASS on this exact version (`01a1cd6c-fd03-70c1-bd06-9ff0417b7698`, x99-codex-luna-design).
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

- `direction-b.html`: the frame as read from doop, unchanged. Its hash is `screens[0].content_sha256` in `LOCK.json`.
- Shows: Approve and Reject as two selectable choices, none chosen when the card opens; one composer that is always there; one primary button whose label follows the choice; plain-language ask, "if you do nothing" line, before → after rows, JSON behind Details, one batch card per goal, a config-change card led by a server-computed diff, applied / not-applied outcomes, the state table, AgentX CLI shape and DevX action template with its failing test.

## Button rule

| Choice | Passkey needed | Button | Enter | Composer label | Agent is woken with |
| --- | --- | --- | --- | --- | --- |
| Nothing | never | Discuss more | sends the message; empty box = cursor goes to the box | Message | outcome `discussion`, the message; the ask stays open |
| Approve | yes | Approve with passkey (passkey icon) | starts the passkey prompt | Note (optional) | outcome `approved`, the note |
| Approve | no | Confirm approval | confirms | Note (optional) | outcome `approved`, the note |
| Reject | never | Confirm Reject | rejects | Reason (optional) | outcome `rejected`, the reason |

- The action's `requiresPasskey` flag picks between the two Approve labels. The proposing agent never sets it. Config actions set it to true.
- Only a chosen Approve of an action that requires it may start a passkey. A typed word is never parsed. Reject needs no passkey.
- Nothing is chosen on open, so Enter can never approve by accident. Tapping the chosen option again clears it.
- `discussion` is a new wake outcome. The ask stays open; the agent answers in the card thread and waits again.

## Implementation must add and test

- Real radio semantics for Approve / Reject: `role="radiogroup"` with arrow-key movement, `aria-checked`, a roving tabindex, and Space on the chosen option clears it. The mockup spans carry `tabindex` and a described keyboard note only; Luna's A11y 5 is for exactly this gap.
- Enter in the composer presses the primary button for the current choice. Shift+Enter adds a line. Discuss more on an empty box focuses the composer and sends nothing.
- Final passkey glyph from the product icon set (`packages/ui/styles` icons, see `AGENTS.md` icon rule). The mockup glyph is a drawn stand-in, not the FIDO mark. The icon stays on passkey actions only.
- Server: the `discussion` outcome and the wake payload; the registry test that every action has a passkey flag, a human template and a fixture; the diff renderer test for config actions.
- Token alignment: the mockup uses raw colours; the code slice maps them to semantic tokens.
- Status is an icon and a word, never colour alone.
- All copy needs every supported translation. Reviewer note: `Confirm rejection` may read better beside `Confirm approval`; Cheese wrote `Confirm Reject`, which the frame keeps.

## Not covered by this lock

- Directions A and C are not locked.
- No live keyboard, passkey or assistive-technology flow was tested (static HTML).
- Light theme, loading, empty and error states are not mocked.
- Density: 13 cards on one tall frame. Reviewers scored it 5; the cards are one state each, not one screen.
