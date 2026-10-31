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
- Producer disposition: `admission_status` and `admission_reason`, copied from what the comment enqueue path returned (`queued`, `coalesced`, `deferred`, `blocked` reason). NULL until it returned, NULL for a trigger with no result, always NULL for wakeups.

## What the columns do not say

- `native_fold_eligible` mirrors the target `MergeCommentIntoQueuedIssueTask` (comments) or `FindPendingWakeupTask` for a queued run (wakeups) would pick. It is read once, without a lock. It does not say a fold happened, and a fold can still lose a race.
- `queue_snapshot` and `active_task_id` are a point read. Nothing infers delivery or coalescing from temporal proximity.
- A missing row is not a no-op. Count completeness per source from the producer tables (`comment` rows whose trigger resolved to a squad leader; receipts) and treat a missing row as a capture gap.

## Joins for 0b

| Question | Join |
| --- | --- |
| Which run covers a comment? | `agent_task_queue` where `trigger_comment_id = e.trigger_comment_id` or `e.trigger_comment_id = ANY(coalesced_comment_ids)`; `delivered_comment_ids` for what a claim embedded. `agent_id = e.agent_id`. |
| Which run did a wakeup input start? | `issue_wakeup_receipt.id = e.receipt_id` (`task_id`, `processed_at`); `agent_task_queue.context->>'wakeup_id' = e.wakeup_id::text`; the wakeup's `activity_log` rows (`wakeup_triggered`, `wakeup_skipped`) hold the outcome; there is no `issue_wakeup_activity` table, and those rows name the rule but not the receipt, so 0b does not join them. Receipts are deleted 7 days after processing: join inside that window or lose the receipt. |
| What did the leader do? | The run's comments (`comment.source_task_id`), status and assignee changes (`activity_log`), and wakeups or issues it created, all keyed by the run id. |
| Window | `(workspace_id, source, event_time)` index. |

Counterfactual 2/5/10-minute coalescing windows come from `event_time`, `native_fold_eligible` and `active_task_id`. They are measurements, never an admission receipt. Slice 0b implements them below.

## Retention

No foreign keys, like `governed_vote_record`: the rows must outlive the receipts, runs and wakeups they describe. Deleting an issue leaves its rows; deleting the workspace removes them (`DeleteWorkspaceLeafData`).

## Slice 0b: joins and report (CHE-1919)

`go run ./cmd/run_gate_report --workspace <uuid> --since 168h [--json]` is read only. It writes nothing, adds no migration and changes no capture path. `service.BuildRunGateReport` does the work; `--json` lists every joined event.

### Joins

| Source | Run link (native evidence only) |
| --- | --- |
| `comment_leader` | `agent_task_queue` of the same issue and agent whose `trigger_comment_id`, `coalesced_comment_ids` or `delivered_comment_ids` holds the comment. Links: `trigger`, `coalesced`, `delivered`. A cancelled duplicate keeps its link; the best cover is first (not cancelled, then delivered, coalesced, trigger, then earliest). |
| `wakeup_pr_*` | `issue_wakeup_receipt.id = receipt_id`, then `receipt.task_id`. One receipt per event (unique on rule, revision and key). |

Timing never links a run. A run that changed something is read from its own stamps:

| Outcome | Evidence | Attribution |
| --- | --- | --- |
| `comment_posted` | `comment.source_task_id` | `task_id` |
| `issue_created` | `issue.origin_type = 'agent_create'`, `origin_id` | `task_id` |
| `wakeup_created` | `issue_wakeup.source_task_id` (a rule deleted since leaves no row) | `task_id` |
| `status_changed`, `assignee_changed` | the agent's `activity_log` rows on the issue inside the run's start to end | `window`; `window_ambiguous` when another run of the same agent on the issue overlaps that instant |

`activity_log` carries no run id, so a status or assignee change is attributed by window and labelled so. Stamping a run id on those rows is a capture change and is not part of 0b.

### Dispositions

Run states: `run_completed_mutated`, `run_completed_mutation_ambiguous` (only an ambiguous window change), `run_completed_no_recorded_mutation`, `run_failed`, `run_cancelled`, `run_in_flight`. `run_completed_no_recorded_mutation` says no outcome was recorded; it is never an audited safe no-op.

Explicit evidence of no run: `no_run_blocked` and `no_run_steered` (the producer's own admission), `no_run_no_receipt` (the producer wrote no receipt), `receipt_consumed_no_run` (reason: the receipt outcome, else `outcome_not_on_receipt`), `receipt_pending`, `receipt_deferred_active_run`.

Unknown, kept distinct: `unknown_no_admission_record` (no run joined and the producer returned nothing), `unknown_run_missing` (the producer or receipt names a run that is gone), `unknown_receipt_missing` (the event names a receipt that no longer exists; reason `wakeup_rule_deleted` when the rule row is gone, since deleting a rule or its issue deletes its receipts at any age, else `wakeup_rule_present` with the cause unknown; expiry is never inferred because a missing receipt carries no `processed_at`), `unknown_admission_value`.

### Completeness and volume

Per source: ledger events, observations (`seen_count` total), native-fold eligible, admission and disposition counts. Producer-side denominator, always a floor and never proof of full capture (`denominator_complete` is false and `limits` says why): comments a squad-leader run is recorded against (`comment_without_run_invisible`: a comment that resolved to a leader but made no run is invisible), and surviving PR receipts (`receipts_deleted_with_rule`: receipts go with their rule or issue at any age, no row records it, so the inputs of a deleted rule leave the denominator even in a recent window; `receipt_retention_7d` is added when the window reaches back past 7 days). `ledger_rule_gone` counts ledger events whose rule no longer exists: they are captured but absent from the denominator. `missing` is surviving producer rows with no ledger row, a lower bound on what was lost. `ledger_first_event_at` marks where capture starts.

### Counterfactual windows

2, 5 and 10 minutes, fixed leading-edge: per (source, issue, agent, head) the first event opens a window, later events inside it fold, one exactly a window later opens the next. Head is part of the group because the native merge is head-scoped. Protected events stand alone and are counted by reason: `explicit_mention` (origin `mention_squad_leader`), `worker_reply`, `no_agent`. For folded events the report also counts those native-fold eligible at capture, reported coalesced by the producer, whose own run acted, and with no joined outcome. Each event in `--json` keeps its identity, native fold eligibility, queue states and active task, and each window records the opener's event key and active task. These are measurements, never delivery receipts and never permission to suppress work.

### Limits

- Real-data acceptance is not claimed here; the join is tested on fixtures against a local database.
- A consumed receipt without a task keeps its outcome only on the wakeup timeline unless the payload holds it (`paused`).
- Status and assignee attribution is by window.
- Not captured, so not reported: `child_done`, `pr_needs_attention`, assignment runs, failure callbacks and PR events dropped before a receipt exists.
