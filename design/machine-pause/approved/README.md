Locked from doop page `CHE-2433 · machine pause` (file https://congvc-c00.taila6fa8a.ts.net:34443/c/mRAnnjvGod), frame `AvexgrxkBp`, `updatedAt` `2026-10-29T15:15:52.443Z`. Issue [CHE-2433](mention://issue/01a186b7-4595-722b-a876-77de56f6cb41).
Direction A selected by Cheese (`01a18c04-779a-7536-884d-a358ef81bb01`, also `01a18c04-37f6-7edd-b432-27499c232126`), UX PASS (`01a18841-cdbb-78ec-86bf-5380ff90e788`).
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

- `machine-card-a.html`: the frame as read from doop, unchanged (system font stack, no external fetch). Its hash is `screens[0].content_sha256` in `LOCK.json`.
- `machine-card-a-1240.webp`: Chrome capture of that file at 1240 px, stored as lossless WebP (pixel-identical to the PNG capture; the PNG was 366KB, over the 300KB image budget).
- Shows, desktop 780 and phone 390: running, reason menu, pausing, drained, offline (paused and not), loading, error, member without rights, ending soon, pause ended (live and offline).
- Button label: the frame says "Delete runtime", the current `runtimes.json` text. Cheese chose no label. A rename to "Remove" edits the frame and needs a new UX score.
