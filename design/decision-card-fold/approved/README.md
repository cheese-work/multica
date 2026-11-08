Locked from doop page `mRAnnjvGod:page1` (file https://congvc-c00.taila6fa8a.ts.net:34443/c/mRAnnjvGod), frame `HtwdZpoCai`, `updatedAt` `2026-11-07T12:47:11.565Z`. Issue [CHE-4432](mention://issue/01a1b605-a1c7-7c53-9994-38ee24d81ab6).
Direction A (one-row header button, question first) selected by Cheese (decision request `b2878829-c391-4dfa-be6d-450d678ccbe3`, recorded as comment `01a1b639-3434-7539-9af6-8231c174af60`; Cheese's reply in the thread is `01a1b63f-5cde-704e-ac9b-7173604209d2`: "A"). UX PASS on this exact version (`01a1b620-dfc0-783a-84cb-8b59adf9f22b`, x99-agy-gemini-ux).
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

- `direction-a.html`: the frame as read from doop, unchanged. Its hash is `screens[0].content_sha256` in `LOCK.json`.
- Shows: DM request cards and issue decision cards folded to one header row (state icon, question, chosen-answer chip, time, chevron) when `picked_up`, `withdrawn` or `disclaimed`; `open` and `not_picked_up` stay expanded. The folded row is one `button` (36 px minimum height, `aria-expanded`, state word as screen-reader text). Expand opens the body under the header in place. Desktop 780 px and phone 390 px (the phone hides the time date and the chip label). The all-answered issue banner uses a neutral frame titled "Decisions" with an "N answered" count.

## Fold rule (D9 = A)

Fold on `picked_up`, `withdrawn`, `disclaimed`. Keep `open` and `not_picked_up` expanded. Done at page load: render folded, no animation. Expand state is not saved.

## Implementation must add and test

- The answer draft and form stay mounted while folded, hidden with `inert`.
- Motion: height and opacity, 200 ms ease-out. `prefers-reduced-motion: reduce` turns it off.
- The state is an icon and a word, never colour alone.
- The all-answered banner copy ("Decisions", "N answered") is new in this mockup. Confirm it in the code slice and add every supported translation.
- The warn text colour `color-mix(in oklch, var(--warn), black 40%)` is a documented non-token exception; check contrast against the real tokens.

## Not covered by this lock

- The proposal page was not created; frames sit on `page1`.
- No local standalone render check was run (NOT-RUN) and no live capture of today's cards was taken (NOT-CAPTURED).
- Dark theme, loading, empty and error states are not mocked.
