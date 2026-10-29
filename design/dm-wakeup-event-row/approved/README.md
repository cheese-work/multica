Locked from doop page `CHE-2459 · fired wakeup event row (test)` page 1 (file https://congvc-c00.taila6fa8a.ts.net:34443/c/LNn7rliuvZ), frame `-Yr2vfvBHn`, `updatedAt` `2026-10-29T11:07:11.362Z`. Issue [CHE-2459](mention://issue/01a18724-074b-7ea4-b106-5cd51319e24c).
Direction C selected by Cheese (`01a18753-e957-7ba2-a312-4400a43808a1`), UX PASS (`01a1875a-c9e1-7656-a1a7-965a9b0d1628`).
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

- `wakeup-row-c.html`: the frame, with Inter embedded (vietnamese and latin subsets, data URIs). This is the only change from the doop frame; its source hash is `screens[0].content_sha256` in `LOCK.json`.
- `wakeup-row-c-1240.png`: Chrome capture of that file at 1240 px.
- Shows, desktop 760 and phone 390: collapsed, expanded (with focus ring), no `--title` fallback, running.
