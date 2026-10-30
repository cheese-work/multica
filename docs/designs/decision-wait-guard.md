# Missing decision-request guard

CHE-2593 adds a pass to the existing leased decision worker. The pass observes
open issues in `need_decision` or with the active Readiness option `gated-human`,
except `in_review` (the linked PR carries the merge ask).
An open decision request suppresses the guard. A wait ending, a request appearing,
or an issue reaching a terminal category clears the episode.

Timing starts at the first worker observation without an open request. After
more than ten minutes, the worker admits one task for the owning squad leader
and records the task ID in the same transaction. A worker restart retains that
receipt. Task publication and runtime notification occur after commit. The task
uses existing leader-callback attribution, runtime and project checks, triage,
stop and run-budget gates. An automatic alert cannot use a review-head budget
exception. The alert does not send a decision card or prove human receipt.

Queue admission runs inside a savepoint. A refusal leaves a retryable route
error and the overdue marker, without a delivery receipt. A later pass retries;
an existing pending task is not silently counted as delivering this alert.
Terminal and merge waits with no episode keys are excluded from the scan.
Existing episode keys keep an issue in the scan until cleanup commits.

Ownership resolves from current squad assignment, then the latest scoped leader
task's squad, then exactly one active squad membership for an agent assignee.
Member assignment retains task provenance. Missing, ambiguous, foreign or archived
owners never fall back to an arbitrary squad. Instead the issue retains
`decision_wait_route_error`, so a later pass can retry when ownership is repaired.

The worker owns these issue metadata keys:

- `decision_wait_started_at`: first observed instant.
- `decision_wait_missing_request`: overdue marker, also set when routing fails.
- `decision_wait_alert_id`: admitted leader task ID, the episode receipt.
- `decision_wait_route_error`: unresolved ownership or admission diagnostic.

Workspace settings can disable the guard or adjust the interval:

```json
{"decision_wait_guard":{"enabled":true,"after_minutes":10}}
```

Omitted values enable the guard with a ten-minute interval. Positive intervals
up to 1440 minutes are accepted; other numeric intervals use the default.
This setting changes alerting only, not authorization. No new migration,
scheduler or agent is required. The brief collector in `multica-dotfiles` counts
the overdue marker and excludes terminal issues and issues with an open request.
