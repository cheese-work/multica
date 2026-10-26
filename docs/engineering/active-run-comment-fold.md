# Fold a comment into the agent's active run (CHE-1442)

Plan only. No code ships until a different-lab reviewer returns PASS (CHE-1442 acceptance 1).

## Problem

A comment for agent A queues a second run when A already has a run in flight.

| A's run state | Today |
| --- | --- |
| `queued` | Folded into that run (CHE-1418, #185). |
| `dispatched` | Registered as a planned input. Replayed as one follow-up run after `CompleteTask` only. |
| `running` | Not "pending" for the dedup index. A new `queued` successor run starts. |

Seed 6 (CHE-955, CHE-833): the assignment run was dispatched in under 1s. The dispatch comment, 1 to 10s later, queued a second run.

Two gaps follow:

1. A comment for a `running` agent always adds a run.
2. `reconcileCommentsOnCompletion` runs on `CompleteTask` only (`handler/daemon.go`). A comment deferred onto a run that then fails or is cancelled gets no follow-up.

## Options

| Option | Adds no new run | Survives run failure | Verdict |
| --- | --- | --- | --- |
| A. Reuse member steering (`steer_task_ids`) | yes | no | Rejected. Needs the author's explicit pick of a run. An agent-authored comment has no such pick. A failed delivery is excluded from reconcile on purpose (`ListReconcilableCommentsForIssueSince`), so it is lost. |
| B. Defer onto the active run, extend reconcile to fail and cancel | no | yes | Rejected. The comment reaches a successor run, not the active run. This is today's outcome with a later start. |
| C. Auto-supplement with release | yes | yes | **Chosen.** |

## Chosen path: auto-supplement with release

Reuse the shipped supplement delivery (`task_supplement`, daemon `task-supplement-v1`). Add an `auto` mode whose undelivered comments go back through the normal trigger pipeline.

```mermaid
sequenceDiagram
    participant C as Comment
    participant R as resolveCommentTriggerEnqueue
    participant T as Active run T (running)
    participant D as Daemon
    participant X as Release (terminal hook + sweeper)
    C->>R: trigger for agent A
    R->>R: queued fold (existing)? yes, done
    R->>R: bind auto supplement to T? (guards below)
    alt bound
        R->>T: task_supplement mode=auto, pending
        T->>D: ClaimNextTaskSupplement
        D-->>T: ack delivered
        Note over T: delivered, excluded from reconcile
    else any guard fails
        R->>R: enqueue successor (existing path)
    end
    T-->>X: run ends failed or cancelled, supplement not delivered
    X->>R: replay comment through normal triggers for A
    R->>R: queued fold or fresh enqueue (one run, deduped)
    X->>X: set released_at
```

### Bind guards

All must hold. Any miss falls through to the existing path, so the change can only remove a successor run.

- Trigger routing already chose agent A. Who is woken, and by which mention, does not change.
- A has a `running` task T on this issue with a `task_supplement_capability` row (old daemons and unsupported providers have none).
- T has the same head SHA and the same squad role (`is_leader_task`, `squad_id`) as the trigger, as in `MergeCommentIntoQueuedIssueTask`.
- T is not a wakeup task (`context->>'wakeup_id' IS NULL`).
- The bind locks T's row, so a terminal transition that wins the race binds nothing.

Order in `resolveCommentTriggerEnqueue`: queued fold, then auto bind, then enqueue.

### Changes

1. Migration, additive, no foreign keys:
   - `task_supplement.mode TEXT NOT NULL DEFAULT 'explicit' CHECK (mode IN ('explicit','auto'))`.
   - New table `task_comment_release (task_id UUID NOT NULL, released_at TIMESTAMPTZ NOT NULL DEFAULT now())` with a unique index on `task_id`, built `CONCURRENTLY` in its own migration. It is a side table because `agent_task_queue` is a hot `SELECT *` table (migration 538 note). One row means "this run's undelivered inputs were handed back to routing".
2. `BindAutoCommentTaskSupplement` query: `BindCommentTaskSupplement` plus the head and role guards, writes `mode='auto'`.
3. `foldCommentIntoRunningTurn` in `handler/comment.go`, wired as above. Notifies the daemon with `NotifyTaskSupplementAvailable`.
4. `ListReconcilableCommentsForIssueSince`:
   - The supplement exclusion becomes `mode='explicit' OR status='delivered'`. An undelivered auto supplement is replayable.
   - The explicit-cancel acknowledgement also matches a comment bound to the cancelled run by a supplement, not only by `trigger_comment_id` or `coalesced_comment_ids`.
   - New argument `planned_only`. When true, only `c.id = ANY(planned_comment_ids)` qualifies. The thread and `created_at > since` window is off.
5. Termination release. Extract the per-comment replay body of `reconcileCommentsOnCompletion` into `replayCommentForAgent`. New `releaseInputsOnTermination(task)`:
   - Builds the run's **undelivered inputs**: `coalesced_comment_ids` plus the trigger comment, minus `delivered_comment_ids`, plus every auto supplement of the run that is not `delivered`.
   - Runs the existing reconcile with `planned_only=true` over that set. A planned input (a comment registered on a `dispatched` run by `registerPlannedCommentForActiveTask`) and an auto supplement take the same path.
   - Replays first, then inserts the `task_comment_release` row (`ON CONFLICT DO NOTHING`). A crash between the two repeats the replay. The pending-task fold absorbs the repeat.
6. Call sites for `releaseInputsOnTermination`:
   - `FailTask` and `CancelTask` handlers.
   - A runtime-sweeper tick (`cmd/server`). It selects failed, cancelled and completed runs with `completed_at` in the last 24h, no release row, and at least one undelivered input. It covers offline-runtime failure, the `CancelTasksForIssue`, `CancelTasksForAgent` and archived-agent cancels, and a crash between commit and release.
   - `CompleteTask` keeps the full reconcile and inserts the release row after it, so the sweeper skips that run.
   - The window stays `planned_only` on failed and cancelled runs. A failed run does not replay every member comment posted during it. Retry belongs to CHE-1126.
7. `ClaimNextTaskSupplement`: resolve the author name for agent authors (today it joins `"user"` only and prints "a user").
8. Trigger outcome: report `coalesced` with a new reason `coalesced_active_run`. Do not add a status.

### Cancellation sources

The existing reconcile already splits cancels by `cancelled_by_type` (`ListReconcilableCommentsForIssueSince`, explicit-cancel clause). The release reuses that predicate without change in meaning.

| Cancel source | `cancelled_by_type` | Undelivered inputs |
| --- | --- | --- |
| A member or an agent cancels this run (`CancelAgentTaskByUser`) | `member` or `agent` | Acknowledged. No replay. |
| Server-initiated: stale plan, claim failure, offline runtime, archived agent (`CancelAgentTask`, `CancelAgentTaskWithReason`) | `system` | Replayed through normal triggers. Routing, permissions and agent state are re-checked, so an archived agent yields a blocked outcome, not a run. |
| Issue deletion (`CancelAgentTasksByIssue`) | `system` | Nothing replays: the issue is gone, so the replay finds no issue and the release row is written. |

There is no member-level "cancel all" path. A member stops runs one task at a time, which is the explicit row. So replay never recreates work right after an intentional stop.

### What does not change

- Mention routing and permissions. Replay re-runs them (`computeCommentAgentTriggers`, `canInvokeAgent`).
- Member steering. `mode='explicit'` keeps its semantics and its exclusion from reconcile.
- Daemon protocol. The server already gates on the capability row.

## Acceptance 3 check

| End of T | Input state | Result |
| --- | --- | --- |
| Completed, delivered | auto supplement `delivered` | Nothing more. |
| Completed, not delivered | supplement `failed turn_ended` | Full reconcile replays: one follow-up run. |
| Failed | auto supplement undelivered, or planned input from `dispatched` | Release replays: one follow-up run. |
| Cancelled by `system` | same | Release replays: one follow-up run. |
| Cancelled by a member or an agent | same | Acknowledged: no run. |
| Any terminal, hook missed | no release row | Sweeper replays once. |
| Provider rejects the input | supplement `failed` (ack) | Release replays when T ends. |

A follow-up after a failed run is not a regression. Today the `queued` successor already exists for a `running` run. For a `dispatched` run the comment is lost on failure today.

## Tests first (red, then green)

Each runs against the handler test database, like `comment_issue_fold_test.go`.

1. Comment for A while A's task is `running` with capability: no new `queued` row, one `task_supplement` with `mode='auto'`, outcome `coalesced_active_run`. Red today: a second `queued` row appears.
2. Dispatched assignment run starts within the same second, then the dispatch comment arrives: one run total after the start.
3. Guard misses each keep the old behavior: no capability row, different head, different squad role, wakeup task.
4. Running run fails with the auto supplement `pending`: one follow-up run for the comment. Red today: none.
5. **Dispatched run fails** with a comment registered as a planned input: one follow-up run. Red today: none.
6. **System cancel** of a `running` run (auto supplement) and of a `dispatched` run (planned input): one follow-up run each.
7. **Explicit cancel** by a member and by an agent, for both input kinds: no follow-up run. Separate test from 6.
8. Run completes with the supplement `delivered`: no follow-up.
9. Sweeper releases a failed run's leftover once. A second tick adds nothing. A crash between replay and the release row leaves one pending run after the retry.
10. A failed run does not replay a member comment that was neither planned nor supplemented.
11. Agent-authored comment shows the agent name at claim.
12. Explicit steering test suite stays green (`comment_steer_test.go`, `task_supplement_test.go`).

## Open questions for the reviewer

1. **Outcome status.** The client allowlist accepts `coalesced` (`packages/core/issues/comment-trigger-outcomes.ts:77`). The RHC-2 wrapper's server-side admission check is not yet read. Step 0 of the code work reads it. If it rejects `coalesced`, report `steered` instead.
2. **Replay window.** A repeat after a crash between replay and release row can run one extra follow-up if the first follow-up already completed. The plan accepts that rare window. A claim-before-replay order would trade it for a lost comment.
3. **Upstream.** `upstream/main` (fetched for this plan) has steering work only: more providers (Kiro `#9396`, Cursor `#9322`), Claude handshake budget (`#9252`), supplement root owners (`#9061`). No rule folds a comment into an active run without the author's pick. The plan reuses that provider gating through the capability row.

The `task_supplement` primary key is `(comment_id, task_id)` (migration 549), so one comment can fold into several agents' runs. No index change is needed.

## Size

About 5h: migrations and queries 1h, bind and wiring 1h, release, sweeper and cancel rules 2h, tests alongside. Over the 4h guide because planned inputs now share the release path.
