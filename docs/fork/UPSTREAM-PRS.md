# Tracked upstream PRs

Ledger for open upstream PRs (CHE-3676 process v2). One row per PR. `scripts/upstream-pr-watch.mjs` (CHE-3958) polls these rows. It never edits this file: what it observes (head, state, watermark, notes) goes into its state file as an overlay, and each continuation carries the exact row for the owning agent to commit. A committed state that is further along than the overlay wins (`open` < `merged upstream` < `closed unmerged` / `integrated into fork main`).

| Column | Meaning |
| --- | --- |
| PR | `owner/repo#number`, for example `multica-ai/multica#1234` |
| Issue | Owning fork issue; its assigned agent gets one continuation per actionable event |
| Head SHA | Upstream PR head at the last poll |
| State | `open`, `merged upstream`, `closed unmerged`, `integrated into fork main` |
| Watermark | Timestamp of the newest event seen (`-` before the first poll) |
| Convergence PR | Fork PR that takes upstream's version after a merge (`owner/repo#number`, `-` until it exists); the watcher reports `integrated into fork main` when it merges |
| Notes | Merge SHA, or close reason |

Add a row when Cheese approves a new upstream PR (scope A, once per PR).

## Run

Owner: the `congvc` user on X99. Unit files are in `deploy/upstream-watch/`; installed copies live in `~/.config/systemd/user/` (`upstream-pr-watch.timer`, every 15 minutes, `Persistent=true`; linger is on). `run.sh` is installed to `~/.local/share/upstream-pr-watch/`; it keeps a private checkout and runs `main`. `UPSTREAM_WATCH_REF` (a systemd drop-in) pins another branch only until `main` carries `scripts/upstream-pr-watch.mjs`, or while that branch still exists; after the watcher PR lands, or if the branch is deleted, `run.sh` uses `main` by itself, so ledger rows merged to `main` are picked up without touching the unit.

```
install -m755 deploy/upstream-watch/run.sh ~/.local/share/upstream-pr-watch/run.sh
cp deploy/upstream-watch/upstream-pr-watch.{service,timer} ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now upstream-pr-watch.timer
```

Check: `systemctl --user list-timers upstream-pr-watch.timer` and `journalctl --user -u upstream-pr-watch -n 20`. Needs `multica` and `node` on `PATH` (the unit sets `~/.local/bin`). The public API needs no token; `GITHUB_TOKEN` only raises the limit.

State file: `~/.local/state/upstream-pr-watch.json`, replaced atomically. It holds the row overlay, per-page ETags, seen event keys, `pending` (the delivery outbox), `backoffUntil` and `stale`. A failed poll, a failed delivery or a rate-limit backoff exits 1 (the unit shows `failed`) and sets `stale` or `backoffUntil`. No actionable event means no agent run.

Delivery: one `multica issue wakeup create --kind at --mode once --agent-id <assignee>` per event batch, carrying a `[upstream-watch:<key>]` marker. The delivery is saved to `pending` before it is attempted and the poll state (ETags, seen keys, overlay) is committed only after it worked; a retry finds the marker on the issue and does not create a second wakeup. The issue must be assigned to an agent.

## Tracked

| PR | Issue | Head SHA | State | Watermark | Convergence PR | Notes |
| --- | --- | --- | --- | --- | --- | --- |
