Locked from doop page `CHE-2684 · banner audiences` (file https://congvc-c00.taila6fa8a.ts.net:34443/c/mRAnnjvGod), frame `PX_YkzqAb-`, `updatedAt` `2026-10-30T16:58:01.161Z`. Issue [CHE-2684](mention://issue/01a18db6-7727-747b-8592-d452a268d21d).
Direction B selected by Cheese (`01a18fda-6a9c-72c5-a3a4-f1bcd67ccea6`, decision request `e8e5a058-1bb5-4afe-b731-aa89fcacc53b`), UX PASS (`01a18dc6-f22a-7ba3-91d6-4f7be87f8388`, x99-codex-luna-design).
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

- `banner-audiences-b.html`: the frame as read from doop, unchanged (system font stack, no external fetch). Its hash is `screens[0].content_sha256` in `LOCK.json`.
- `banner-audiences-b-1240.webp`: Chrome capture of that file at 1240 px, stored as lossless WebP.
- Shows, desktop 780 and phone 390: single machine for the daemon owner (Resume) and for a project lead (no Resume, cause and wait count), several machines collapsed and with Details open (Resume only where the viewer holds pause rights), a new event after dismiss (banner returns, new row tagged), dismissed (header icon keeps the count), loading or error (nothing renders), and the header icon states (none, banner open, dismissed, focus with tooltip).
- Not shown, implement as written in the frame notes: counts above 9 read `9+`; the icon sits at the right end of `PageHeader` before the page actions; accessible names are `Hide machine banner: <summary>` and `Show machine banner: <summary>` with `aria-pressed`.
