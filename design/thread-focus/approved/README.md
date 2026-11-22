Locked from doop page `che-6385` (file https://congvc-c00.taila6fa8a.ts.net:34443/c/mRAnnjvGod), 25 frames at the exact versions listed with ids and `updatedAt` in `LOCK.json`. Prototype start: https://congvc-c00.taila6fa8a.ts.net:34443/c/mRAnnjvGod/proto?frame=kpQ01u8mPy. Issue [CHE-6446](mention://issue/01a1e44c-15ab-79b7-b877-919fbe89a82a); plan [CHE-6385](mention://issue/01a1e418-e3a9-73dd-b748-0166a0dfc976).
Direction B (Follow default, unread marker only) approved by Cheese's recorded pick (CHE-6446 comment `01a1ef32-0a5c-7658-a241-c1e8e0bcd966`). Mute, pin and the one-agent filter were dropped (U6, CHE-6442, cancelled).
UX verdict `01a1f18c-f1f2-731a-b945-f49091c42d15` (x99-codex-terra-ux, different lab) on these exact versions is **HOLD, not PASS**: scores 7 to 8 of 10 on all six criteria and no blocking lint finding; HOLD only because the automatic checks G3 `check_design_completeness`, CHE-6853, CHE-6854 and CHE-6628 are not live. Cheese waived those four checks for exactly these versions on decision card `01a1f1a0-aed2-74d2-98c0-b79165d2b3fd`, answer `01a1f1ac-7353-7055-a64a-e786420740a4` (19 Nov 2026, 09:39 ICT). A frame edited after these versions is outside the waiver and needs its own re-score.
Approved when the lock PR merges. The topic's last implementing PR deletes the `*.html` reference files; `screens/` stays as the design reasoning record.

## What this lock holds

- Screens (1440 px desktop and 390 px phone in each file): B-a Follow default with the quiet view line; B-b a folded agent row opened in place and the link row; B-c the View popover, per-issue override and unread marker; B-d and B-d2 Focus for this issue; B-e All for this issue; B-f loading, nothing waits, relation error, disconnected; B-g and B-h the Next waiting pill at 1 of 2 and 2 of 2; S-a decision card placement; S-b ask thread; S-c pick submitted.
- `kpQ01u8mPy` is the index and prototype start. `taTwiY7Dx_` is the tokens frame, `_W3uHQuwdF` the components, `hmmC1Pq87b` and `7n7qZ6aX4K` the motion spec and its played frame. The eight `* · doc` frames are copied to `screens/*.md`.
- The decision card itself is the locked CHE-5480 / B v9 card (`design/decision-screen/approved/`, `design/decision-card/approved/`); only its placement is drawn here.

## How the files were exported

Each `<frame id>.html` is the `html` field of the doop MCP `get_frame` call, written as returned, plus one trailing newline. `LOCK.json` `content_sha256` is the sha256 of that file. The same 25 frames were exported independently by x99-codex-terra-ux on 19 Nov 2026 09:03 ICT for the verdict; the sorted `sha256sum` list of both exports is identical (`0af1faa3f6dd2aabc36275f7345a3f647b3f680c89d1d45d29e368c29dcff418`, 25 of 25 files).

## Carry into implementation (not blockers)

- Motion, screen-doc, sloplint and components checks were read by a person only (waived above). Keyboard and screen-reader behaviour was not live-tested (live check waived 18 Nov 2026 16:44 ICT).
- Map mockup colour values to semantic tokens in `packages/ui/styles/tokens.css`; the mockup copies values.
- All copy needs every supported translation. The mockup is in English.
