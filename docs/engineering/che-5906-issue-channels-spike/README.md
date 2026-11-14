# CHE-5906: issue channels spike C1 (plan v2 of CHE-5673)

Answers Q1 to Q3 and holds the failing tests for C2 and C5.

## Q1: can existing rows answer the channel-list query?

**No. C2 needs the additive `thread_participant` table, so C2 splits into C2-table and C2-api.**

Two reasons:

1. **"Opened" has no store.** Comment authors and `mentioned` Inbox items exist. Nothing records that a member opened a thread in the frame. The joined rule (plan 3.3) cannot be computed from existing rows.
2. **Latency misses the budget at default settings.** Existing-rows query (`measure/channel_list.sql`), 1 connection, 60 runs, synthetic data (10k issues, 1.5M comments, 426k inbox items; the member has 4,075 own comments and 4,531 tagged issues):

| Variant | p50 | p95 |
| --- | --- | --- |
| JIT on (Postgres default), 7 / 30 / 90 day window, with a new partial index on comment author | 701 / 1122 / 1270 ms | 740 / 1243 / 1346 ms |
| JIT off, new index, 7 / 30 / 90 days | 19 / 83 / 242 ms | 24 / 118 / 280 ms |
| JIT off, no new index (n=5, thin), 7 / 30 days | 146 / 181 ms | 159 / 203 ms |

Reading: existing rows pass 200 ms p95 only with JIT off, a new comment-author index and a window of 30 days or less. A 90-day window fails. A 30-day window also drops older joined threads. JIT is a server-wide setting, so a per-query fix needs `SET LOCAL jit = off`.

Table read path (`measure/channel_list_table.sql`, 267,624 rows, 18,183 joined for the benchmark member): p50 26 ms, p95 30 ms (JIT on), p95 33 ms (JIT off). No window needed.

### Table spec

`measure/thread_participant.sql` holds the DDL. Summary:

- Key `(workspace_id, member_id, root_comment_id)`; columns `issue_id`, `wrote_at`, `tagged_at`, `opened_at`, `joined`, `last_activity_at`, `waiting`.
- Index `(workspace_id, member_id, last_activity_at DESC) WHERE joined`, and `(root_comment_id)`.
- Both indexes are `CREATE INDEX CONCURRENTLY`, one statement per migration file.
- Writers: the comment write path (author, tagged members, last activity), the thread-open endpoint (`opened_at`), the reply that completes a tag (sets `joined`). A backfill from comments and `mentioned` Inbox items fills history.
- `notify_only` tags never set `waiting` (see Q2).

## Q2: is `notify_only` stored?

**No. It is a request field only.**

- Read from the request body: `server/internal/handler/comment.go:1497`, `:1759`, `:1957` (`NotifyOnly`).
- It reaches the `comment:created` event (`commentEvent`, `comment.go` about `:2155-2168`) but the comment row has no column.
- `notifyMentionedMembers` (`server/cmd/server/notification_listeners.go:585`) writes `mentioned` Inbox items with `details {comment_id, mention_source}`. No `notify_only`.
- Effect: "waiting for you" cannot exclude `notify_only` tags after the request ends.
- Fix for C2-table or C4: carry `notify_only` in the event payload and write it to the Inbox `details` and to `thread_participant` (tag stored with `waiting = false`). The C2 red test pins `details.notify_only`.

## Q3: runs per thread over 7 days, and M

Source: 4,123 runs with `trigger_comment_id` over 7 days, mapped to 2,454 threads by walking `parent_id` to the root (`GetThreadRoot` semantics); 0 runs unmapped. Triggers: 3,991 agent comments, 131 member comments, 1 system. Run rows include retries.

| | n threads | p50 | p95 | p99 | max |
| --- | --- | --- | --- | --- | --- |
| All runs | 2,454 | 1 | 4 | 7 | 26 |
| Max in any rolling 24 h | 2,454 | 1 | 4 | 7 | 26 |
| Member-comment triggers only | 82 | 1 | 4 | 5.2 | 6 |

Threads over M in 24 h: M=3 181, M=4 108, M=5 58, M=6 38, M=8 17, M=10 10, M=15 1, M=30 0 (of 2,454).

**Proposed M = 8** for `warn`: it flags 17 of 2,454 threads (0.7%) and sits above p99 (7). The literal p95 (4) would flag 108 threads (4.4%), too noisy for a gate that later becomes `block`. Review M after one week of `warn` data, as plan 3.4 requires. Member-only threads peaked at 6 runs, so M = 8 leaves member threads untouched today.

## Caveats

- Synthetic benchmark data; one connection; a real mix of load may differ.
- Run rows include retries, so runs per thread is an upper bound.
- Decision-ask "waiting" was not benchmarked.
- The benchmark checks workspace membership only, not a per-issue read check. C2 must name the read check.
- The 7-day window uses `created_at`.

## Failing tests (build tag `issue_channels_red`, so main CI skips them)

- C5: `go test -tags issue_channels_red -run TestThreadRunCapC5 ./internal/handler/` (`server/internal/handler/thread_run_cap_c5_red_test.go`). Output: `red-output/c5-red.txt`. 4 subtests red, 1 (`warn`) green as a guard.
- C2: `go test -tags issue_channels_red -run TestChannelListC2 ./cmd/server/` (`server/cmd/server/channel_list_c2_red_test.go`). Output: `red-output/c2-red.txt`. Red: `GET /api/channels = 404`. The first assertion aborts the test, so the subtests list the cases C2 must pass once the route exists.
- Not in the C2 test: "opened" (no store yet), unreadable issue (read check unnamed), summary rows.
- Green: none yet. Implementation belongs to C2 and C5.

## Reproduce

`measure/` holds the seed and query SQL, `run_bench.sh` (pgbench, 60 runs), and the fetch and analysis scripts for Q3. `runs_per_thread.py` reads `data/comment_runs.json` and `data/comments/` from the working directory, which the fetch scripts write.
