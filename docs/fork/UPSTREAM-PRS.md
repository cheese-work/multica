# Tracked upstream PRs

Ledger for open upstream PRs (CHE-3676 process v2). One row per PR. `scripts/upstream-pr-watch.mjs` (CHE-3958) polls these rows and updates `Head SHA`, `State`, `Watermark` and `Notes`.

| Column | Meaning |
| --- | --- |
| PR | `owner/repo#number`, for example `multica-ai/multica#1234` |
| Issue | Owning fork issue; the watcher starts one continuation there per actionable event |
| Head SHA | Upstream PR head, refreshed each poll |
| State | `open`, `merged upstream`, `closed unmerged`, `integrated into fork main` |
| Watermark | Timestamp of the newest event seen (`-` before the first poll) |
| Convergence PR | Fork PR that takes upstream's version after a merge (`owner/repo#number`, `-` until it exists); the watcher marks `integrated into fork main` when it merges |
| Notes | Merge SHA, or close reason |

Add a row when Cheese approves a new upstream PR (scope A, once per PR). `merged upstream` and `closed unmerged` rows need a ledger commit by the owning agent; the continuation says so.

## Run

Every 15 minutes on X99 (needs `multica` on `PATH`; the public API needs no token, `GITHUB_TOKEN` only raises the limit):

```
*/15 * * * * cd <multica checkout of main> && node scripts/upstream-pr-watch.mjs
```

A failed poll or a rate-limit backoff exits 1 and writes `stale` (or `backoffUntil`) into the state file (`~/.local/state/upstream-pr-watch.json`). No actionable event means no agent run.

## Tracked

| PR | Issue | Head SHA | State | Watermark | Convergence PR | Notes |
| --- | --- | --- | --- | --- | --- | --- |
