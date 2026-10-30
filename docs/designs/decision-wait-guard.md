# Missing decision-request guard

CHE-2593 adds a pass to the existing leased decision worker. The pass observes
open issues in `need_decision` or with the active Readiness option `gated-human`.
An open decision request suppresses the guard. A wait ending, a request appearing,
or an issue reaching a terminal category clears the episode.

Timing starts at the first worker observation without an open request. After
more than ten minutes, the worker inserts one inbox alert for the owning squad
leader and records its ID in the same transaction. A worker restart retains that
receipt. The inbox event is published after commit. An inbox alert does not start
a leader task, send a decision card or prove human receipt.

Ownership resolves from current squad assignment, then the latest scoped leader
task's squad, then exactly one active squad membership for an agent assignee.
Member assignment retains task provenance. Missing, ambiguous, foreign or archived
owners never fall back to an arbitrary squad. Instead the issue retains
`decision_wait_route_error`, so a later pass can retry when ownership is repaired.

The worker owns these issue metadata keys:

- `decision_wait_started_at`: first observed instant.
- `decision_wait_missing_request`: overdue marker, also set when routing fails.
- `decision_wait_alert_id`: durable deduplication receipt for the episode.
- `decision_wait_route_error`: unresolved ownership diagnostic.

Workspace settings can disable the guard or adjust the interval:

```json
{"decision_wait_guard":{"enabled":true,"after_minutes":10}}
```

Omitted values enable the guard with a ten-minute interval. Positive intervals
up to 1440 minutes are accepted; other numeric intervals use the default.
This setting changes alerting only, not authorization. No new migration,
scheduler or agent is required. The brief collector in `multica-dotfiles` counts
the overdue marker and excludes terminal issues and issues with an open request.
