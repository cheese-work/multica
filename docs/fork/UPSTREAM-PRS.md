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
| Notes | Merge SHA, or close reason (the agent writes it after reading the PR thread; the watcher never copies outside text into a row) |

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
| multica-ai/multica#10111 | CHE-3959 | 5ab8141e454acd77bfa494ab80202af1f1232752 | merged upstream | - | cheese-work/multica#1311 | merge `64e62a1253`; maintainer added 404 guard |

## Upstream PR tracking (multica-ai/multica, author congvc-dev)

Refreshed by CHE-4789. Public GitHub API read 2026-11-09 ICT (anonymous REST: pulls, issue comments, reviews, review comments, events). Each PR was opened by `congvc-dev`; it is not a bot comment. `first response` = the first comment or review from an account other than the author and other than `vercel[bot]`. `review rounds` = formal review submissions (a `CHANGES_REQUESTED` then `APPROVED` pair counts as 2). Times are ICT. A person-readable time carries `ICT`; the offset after it is time since opening.

| Ledger ID | Upstream PR | Opened | First response | Merged/closed | Time to merge | Review rounds |
| --- | --- | --- | --- | --- | --- | --- |
| FX045 (fork #199) | [#10111](https://github.com/multica-ai/multica/pull/10111) refetch a failed issue mention on window focus | 2026-11-08 18:30 ICT | 2026-11-08 19:04 ICT (+34 min, `multica-eve` comment asking for screenshots) | merged 2026-11-09 15:13 ICT (squash `64e62a1253`) | 20 h 43 min | 0 formal reviews, +1 maintainer follow-up commit (`5ab8141e45`, 404 guard) |
| FX023 (fork #93) | [#8615](https://github.com/multica-ai/multica/pull/8615) narrow protocol-lint waiver-claim regex | 2026-09-21 10:15 ICT | none | closed unmerged by author 2026-09-21 17:26 ICT | - | 0 |
| - | [#8093](https://github.com/multica-ai/multica/pull/8093) multiple GitHub app installations per workspace | 2026-09-07 10:24 ICT | none | closed unmerged by author 2026-09-07 23:28 ICT | - | 0 |
| - | [#2090](https://github.com/multica-ai/multica/pull/2090) customizable workflow status substrate | 2026-05-05 17:04 ICT | none | closed unmerged by author 2026-05-05 17:22 ICT | - | 0 |
| - | [#2089](https://github.com/multica-ai/multica/pull/2089) driver-plane verdict routing state machine | 2026-05-05 14:39 ICT | none | closed unmerged by author 2026-05-05 14:42 ICT | - | 0 |
| - | [#2012](https://github.com/multica-ai/multica/pull/2012) desktop loads runtime self-host config | 2026-05-03 00:11 ICT | 2026-05-03 10:12 ICT (+10 h 1 min, `Bohan-J` review: changes requested) | merged 2026-05-06 10:39 ICT by `Bohan-J` | 3 d 10 h 28 min | 2 |
| - | [#2003](https://github.com/multica-ai/multica/pull/2003) honor Hermes resume replacement session | 2026-05-02 22:00 ICT | 2026-07-17 14:14 ICT (+75 d 16 h, `Bohan-J` comment: superseded by upstream #2070 / #4015) | closed unmerged by maintainer `Bohan-J` 2026-07-17 14:14 ICT | - | 0 |

Totals: 7 PRs. 2 merged (#2012, #10111). 5 closed unmerged (4 closed by the author, 1 by a maintainer as superseded). 0 open. Median first response over the 3 PRs that had one: 10 h 1 min. Median time to merge: 2 d 3 h 36 min (n=2: 3 d 10 h 28 min, 20 h 43 min). Maintainer follow-up commits before merge: 1 (#10111). Count them with `+N maintainer follow-up commit` in the Review rounds column.

`-` in the ledger ID column = no ledger row verified from the PR body or code. Title similarity is not evidence, so #8093, #2090, #2089, #2012 and #2003 stay unmapped. FX045 and FX023 map because the fork fix PRs (#199 and #93) fix the same defect as the upstream PR: #10111's body describes the failed mention fetch that stays identifier-only (FX045), and #8615's title carries CHE-681, which #93 also carries (FX023). `#10111` is on the Tracked table since CHE-4843 (Cheese approved it on the FX045 card, CHE-3959/CHE-4641). The watcher was blind to it until then because no publish step added the row.
