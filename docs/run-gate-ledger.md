# Run gate shadow ledger (CHE-1918)

`run_gate_event` (migration 670) records the inputs that may start a squad-leader run. It is the capture half of CHE-1917 Slice 0 (plan: CHE-1886, Sol B' v2). Slice 0b joins it to real outcomes and reports.

**Capture only.** Nothing reads this table to admit, fold, delay or skip a run. A failed write is logged (`run gate capture failed; the event continues unchanged`) and dropped; the comment, receipt or run it describes is untouched. Comment text is never stored.

## Sources

| `source` | Producer | Captured at | `event_key` | `candidate_key` |
| --- | --- | --- | --- | --- |
| `comment_leader` | `triggerTasksForComment` (`handler/comment.go`), every trigger with a squad: assignee fallback, `@squad` mention, thread parent, conversation continuation, worker reply routed back to a leader | After triggers are resolved, before steering, folding or enqueueing | comment id | leader agent id |
| `wakeup_pr_merged` | `TriggerPullRequestWakeup` (`service/issue_wakeup_system.go`) | Same transaction as the receipt, behind a savepoint; also on a redelivery | `issue_wakeup_pr_event` key `github:owner/repo#N` | wakeup id |
| `wakeup_pr_checks_failed` | same | same | `github:owner/repo#N:<head sha>` | wakeup id |

Not captured: `child_done`, `pr_needs_attention`, assignment runs, failure callbacks, and PR events the producer drops before a receipt exists (rule off, issue inactive, filtered, malformed settings).

## Columns 0b needs

- Identity: `(source, issue_id, event_key, candidate_key)` is unique. A repeat of the same event bumps `seen_count` and `last_seen_at`; the first snapshot stays.
- Typed origin: `origin` (comment: the trigger source; wakeup: `system_wakeup`), `author_type`, `non_leader_agent_reply`, `system_rule`, `wakeup_id`, `wakeup_revision`.
- State pins: `issue_revision`, `issue_status`, `pr_head_sha` (comment: the issue's reviewed head; wakeup: the head on the PR event, NULL for a merge that carries none).
- Time: `event_time` (comment: `comment.created_at`; wakeup: when the input was recorded).
- Queue at capture: `queue_snapshot` (live tasks of `agent_id` on the issue, newest first, at most 20), `active_task_id` (the first dispatched, running or waiting one), `native_fold_eligible` and `native_fold_task_id`.
- Producer disposition: `admission_status` and `admission_reason`, copied from what the comment enqueue path returned (`queued`, `coalesced`, `deferred`, `blocked`, `jev_routed` reason). NULL until it returned, NULL for a trigger with no result, always NULL for wakeups.

## What the columns do not say

- `native_fold_eligible` mirrors the target `MergeCommentIntoQueuedIssueTask` (comments) or `FindPendingWakeupTask` for a queued run (wakeups) would pick. It is read once, without a lock. It does not say a fold happened, and a fold can still lose a race.
- `queue_snapshot` and `active_task_id` are a point read. Nothing infers delivery or coalescing from temporal proximity.
- A missing row is not a no-op. Count completeness per source from the producer tables (`comment` rows whose trigger resolved to a squad leader; receipts) and treat a missing row as a capture gap.

## Joins for 0b

| Question | Join |
| --- | --- |
| Which run covers a comment? | `agent_task_queue` where `trigger_comment_id = e.trigger_comment_id` or `e.trigger_comment_id = ANY(coalesced_comment_ids)`; `delivered_comment_ids` for what a claim embedded. `agent_id = e.agent_id`. |
| Which run did a wakeup input start? | `issue_wakeup_receipt.id = e.receipt_id` (`task_id`, `processed_at`); `agent_task_queue.context->>'wakeup_id' = e.wakeup_id::text`; `issue_wakeup_activity` for the recorded outcome. Receipts are deleted 7 days after processing: join inside that window or lose the receipt. |
| What did the leader do? | The run's comments (`comment.source_task_id`), status and assignee changes (`activity_log`), and wakeups or issues it created, all keyed by the run id. |
| Window | `(workspace_id, source, event_time)` index. |

Counterfactual 2/5/10-minute coalescing windows come from `event_time`, `native_fold_eligible` and `active_task_id`. They are measurements, never an admission receipt.

## Retention

No foreign keys, like `governed_vote_record`: the rows must outlive the receipts, runs and wakeups they describe. Deleting an issue leaves its rows; deleting the workspace removes them (`DeleteWorkspaceLeafData`).
