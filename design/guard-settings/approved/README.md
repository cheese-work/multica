Locked from doop page `-tt0wuFf9G:page1` (file https://congvc-c00.taila6fa8a.ts.net:34443/c/-tt0wuFf9G), frames `-CvWcUKPBL` (`2026-11-14T14:14:19.025Z`), `wiM6JcN7p-` (`2026-11-14T14:15:46.895Z`), `dh42ccKjU3` (`2026-11-14T14:20:08.628Z`) and `58qKMeIbQ5` (`2026-11-14T14:18:57.500Z`); times 14 Nov 2026 21:14, 21:15, 21:20 and 21:18 ICT. Issue [CHE-5207](mention://issue/01a1cac2-0ed6-7a10-a814-411d23e2b1b4).
Direction B (master-detail). Cheese chose B (CHE-5207 comment `01a1da4e-007e-71f9-b98f-ae5d91db310b`, 14 Nov 2026 20:45 ICT), recorded in comment `01a1da60-cace-7909-a031-6c2f860cad44`. UX PASS on these exact versions (`01a1da73-145c-7a26-b623-5f86d2fff496`, x99-codex-luna-design; every criterion 7 to 8).
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

## What this lock holds

- `guard-settings-main.html`: Guard tab at 1440 px. Left: plugin list with Fixed / Refused / Errors counts for the last 7 days, status dot, action pill (Fix, Deny, Locked), then the review-policy rows. Right: the selected plugin with matches, action, message to the agent, error behaviour, on/off switch, hits-per-day chart and recent decisions. A footer shows the same data in the CLI (`multica guard plugin get`, `multica guard log`).
- `states.html`: loading, empty, error (502), disconnected daemon, read-only member, and save conflict (409).
- `narrow-list-390.html` and `narrow-detail-390.html`: the two steps of the drill-in at 390 px. The side nav becomes a "‹ Settings / Guard" bar, and the detail has a "‹ All plugins" back link.
- Switches are native `<button role="switch" aria-checked>` with labels. Read-only switches are `disabled`. Every control has a 2 px `:focus-visible` outline. A skip link jumps to the main region.

## Implementation must add and test

- Narrow recent-decisions table (Luna's note, the biggest problem): it scrolls sideways inside a card. Keep the region focusable (`tabindex=0`) and named (`role="region"`, `aria-label`).
- Wire the drill-in and the back link: the two 390 px frames are static, and `data-view` only hides one half in CSS. Return must restore the list position and the selected row.
- Passkey edit flow, the 409 re-apply, and live counts are not in the mockup. Counts come from `multica guard` data; the UI and the CLI must show the same numbers.
- Test keyboard order, switch toggling with Space, and screen-reader names. The review was on static HTML only.
- Touch targets are at least 44 px at phone width. The switch grows its hit area with a pseudo-element, not its size; verify the area in the build.
- Map the mockup's colour values to semantic tokens in `packages/ui/styles/tokens.css`; the mockup copies values and the code must not.
- All copy needs every supported translation. The mockup is in English.

## Not covered by this lock

- Directions A and C on the same canvas; they stay as history.
- Dark theme is not mocked.
- Live data, long plugin names, and reflow between 390 px and 1440 px are not verified.
- Backend behaviour of `multica guard` and the passkey flow.
