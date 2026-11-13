Locked from doop page `aBJFsygiKT:page1` (file https://congvc-c00.taila6fa8a.ts.net:34443/c/aBJFsygiKT), frame `Z3bizs5RyO`, version B v9, scored at `updatedAt` `2026-11-12T04:18:27.430Z` (12 Nov 2026 11:18 ICT). Issue [CHE-5162](mention://issue/01a1c949-4c66-744f-a7e4-8a45dcaa594a).
Direction B v9. Cheese chose "Chọn B v9, B thay fold A" on decision card `c482d36e` (13 Nov 2026 08:27 ICT), recorded in CHE-5162 comment `01a1d284-a560-7d3e-86ea-e6fe32ad5e50`. UX PASS on this exact version (`01a1cdfe-0045-7601-a4e0-a94d9d82ea6e`, x99-codex-luna-design: Clarity 8, Hierarchy 8, States 9, Consistency 8, A11y 5, Density 5).
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

## Fold direction A is superseded

B v9 replaces fold direction A of CHE-4431 (design PR 1183, `decision-card-fold` row in `DESIGN.md`). Both CHE-4431 PRs are already merged: design PR 1183 and code PR 1271. Nothing is left to close. The build slices under CHE-2949 must re-scope the merged fold code (PR 1271) onto B v9: B v9 is the only decided-card design from this lock on.

## What this lock replaces

It replaces the v4 lock in this folder (`updatedAt` `2026-11-12T01:38:19.680Z`, UX verdict `01a1cd6c-fd03-70c1-bd06-9ff0417b7698`). The v4 picks `01a1cd63-7807-7887-ae08-22e9e410cf81` and `01a1cce0-7263-747f-ba85-d09e4ac44ae4` are superseded by the 13 Nov 2026 pick above.

- `direction-b.html`: the standalone export of the frame, unchanged (59468 bytes). Its hash is `screens[0].content_sha256` in `LOCK.json`. The live frame now reads a later `updatedAt` (`2026-11-12T10:42:54.370Z`) only because doop added eight `data-doop-id` attributes; without them the HTML is byte-identical to this file.
- Shows: Approve and Reject as two selectable choices, none chosen when the card opens, one joined control, also in a batch; one composer that is always there; one primary button whose label follows the choice and ends with its send key (`(⌘ + ⏎)` on macOS, `(Ctrl + ⏎)` on Windows and Linux); plain-language ask, "if you do nothing" line, before → after rows, JSON behind Details; a config-change card that shows the diff only (old value struck through, new value in a chip) with Show full; decided cards collapsed to one block with Revoke while revocable; a collapse animation that respects reduced motion; one request as one record with a DM view and an issue view; one batch card per goal; the state table; AgentX CLI shape and DevX action template with its failing test.

## Button rule

| Choice | Passkey needed | Button | ⌘ + ⏎ (Ctrl + ⏎) | Composer label | Agent is woken with |
| --- | --- | --- | --- | --- | --- |
| Nothing | never | Discuss more | sends the message; empty box = cursor goes to the box | Message | outcome `discussion`, the message; the ask stays open |
| Approve | yes | Approve with passkey (passkey icon) | starts the passkey prompt | Note (optional) | outcome `approved`, the note |
| Approve | no | Confirm approval | confirms | Note (optional) | outcome `approved`, the note |
| Reject | never | Confirm Reject | rejects | Reason (optional) | outcome `rejected`, the reason |

- The action's `requiresPasskey` flag picks between the two Approve labels. The proposing agent never sets it. Config actions set it to true.
- Only a chosen Approve of an action that requires it may start a passkey. A typed word is never parsed. Reject needs no passkey.
- Nothing is chosen on open, so no key press can approve by accident: plain Enter only adds a line and sending needs ⌘ + ⏎ (Ctrl + ⏎). Tapping the chosen option again clears it.
- `discussion` is a new wake outcome. The ask stays open; the agent answers in the card thread and waits again.

## Implementation must add and test

- Real radio semantics for Approve / Reject: `role="radiogroup"` with arrow-key movement, `aria-checked`, a roving tabindex, and Space on the chosen option clears it. The mockup spans carry `tabindex` and a described keyboard note only; Luna's A11y 5 is for exactly this gap.
- ⌘/Ctrl+Enter in the composer presses the primary button for the current choice. Plain Enter adds a line (the v9 frame header: nothing approves by accident). Discuss more on an empty box focuses the composer and sends nothing.
- Final passkey glyph from the product icon set (`packages/ui/styles` icons, see `AGENTS.md` icon rule). The mockup glyph is a drawn stand-in, not the FIDO mark. The icon stays on passkey actions only.
- Server: the `discussion` outcome and the wake payload; the registry test that every action has a passkey flag, a human template and a fixture; the diff renderer test for config actions.
- Config card: show the diff only; `Show full` opens the changed section as written in the config file, with no − or + marks. Decided cards collapse to one block (question, decision, effect, times, who, Revoke while revocable). The collapse animation honours `prefers-reduced-motion`.
- Send key text follows the platform: `(⌘ + ⏎)` on macOS, `(Ctrl + ⏎)` elsewhere. Plain Enter in the composer only adds a line.
- Re-scope the merged fold code (PR 1271) onto B v9; reuse it where it matches, replace it where it does not.
- Token alignment: the mockup uses raw colours; the code slice maps them to semantic tokens.
- Status is an icon and a word, never colour alone.
- All copy needs every supported translation. Reviewer note: `Confirm rejection` may read better beside `Confirm approval`; Cheese wrote `Confirm Reject`, which the frame keeps.

## Not covered by this lock

- Directions A and C are not locked.
- No live keyboard, passkey or assistive-technology flow was tested (static HTML).
- Light theme, loading, empty and error states are not mocked.
- Density: 13 cards on one tall frame. Reviewers scored it 5; the cards are one state each, not one screen.
