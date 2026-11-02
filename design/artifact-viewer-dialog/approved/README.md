Locked from doop page `MJ4yJPTHt6:page1` (file https://congvc-c00.taila6fa8a.ts.net:34443/c/MJ4yJPTHt6), frame `Bi32qQ55Hi`, `updatedAt` `2026-11-01T04:43:09.622Z`. Issue [CHE-2810](mention://issue/01a19507-594a-75b7-9f0e-6705413bc14d). Builds on the locked card `design/you-are-approving-card/approved/`.
Direction B (dialog replaces the inline Changes / Preview strip) selected by Cheese (decision record `01a19580-8cd4-7537-bd61-7b94239f23b8`, answer `01a19576-8109-7610-bff9-07383eb83be6` to request `ed911608-f115-4f1b-b455-aef59abf3448`), UX PASS on this exact version (`01a19570-7789-7e31-b745-f95f1c476323`, x99-agy-gemini-ux).
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

- `artifact-dialog-b.html`: the frame as read from doop, unchanged. Its hash is `screens[0].content_sha256` in `LOCK.json`.
- Shows: the card with no inline strip (desktop 540 light, dark with focus ring, phone 358); desktop dialog with Changes (light) and Preview (dark); loading and failed states; phone 390 fullscreen with Changes, Preview, failed and first revision (no Changes tab).

## Implementation must add and test

The mockup describes these but cannot show them:

- The file row is one button with `aria-haspopup="dialog"` and a one-line summary ("Preview", "Changes +3 −2"). Download stays on the card.
- Esc, the close button and browser Back close the dialog; focus returns to the row.
- Tab and panel linkage: each `role="tab"` has `aria-controls` to its `role="tabpanel"`, and the panel has `aria-labelledby` back. Arrow keys, Home and End move between tabs.
- Scroll regions stay focusable (`tabindex="0"`) with labels. A long file scrolls inside the region, not the dialog.
- Plain-text files get a Preview tab; a first revision has no Changes tab.
