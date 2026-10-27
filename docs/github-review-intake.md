# GitHub review webhook intake

`POST /api/webhooks/github` durably records `pull_request_review`,
`pull_request_review_comment`, and `issue_comment` deliveries. This does not
change the GitHub App's subscriptions. Those changes remain a separate owner action.

## Acknowledgement and storage

The handler verifies `X-Hub-Signature-256` against the exact request bytes before
opening a database transaction. A bad signature returns `401` without a row.
An accepted intake request requires `X-GitHub-Delivery` (`400` when absent).

One transaction writes:

- `github_webhook_intake`: the delivery GUID, event, signature, original bytes,
  receive time, state, and reason. A unique index makes delivery GUIDs idempotent.
- `github_webhook_revision`: workspace-scoped, normalized revision metadata and
  an initial disposition. A unique index on workspace, object kind, object ID,
  and SHA-256 body hash makes different GUIDs for one revision share one work item.

Only a successful commit returns `202`. A database error returns `500` and
rolls back both writes. Reusing a GUID with different signed bytes or a
different event returns `409` and preserves the original delivery. Every new
GUID retains its own signed bytes, even when its revision is a duplicate.

Malformed signed payloads, unsupported actions, non-PR issue comments and
unknown installations are retained with a durable denial reason. Valid
installations fan out to their bound workspaces in the same transaction.
Workspace membership uses the numeric author ID in `github_review_members`,
not the author's login. Unmapped, departed and bot authors are denied.
Fork PRs and PRs without linked issues are denied. Missing PR metadata is
deferred, never treated as proof that a PR is not a fork.

Denials and deferrals log only routing identifiers, states and reason codes.
GitHub comment or review text is not copied to logs or issue comments.

## Notify-only boundary

This slice creates no issue comment, agent mention or run, including for a
mapped member. The former synchronous CHE-2019 review wakeup is not called by
the webhook endpoint. Its internal helpers remain dormant.

The schema permits `denied`, `deferred`, `quarantined`, `unsigned`, `projected`
and `dispatched`. This handler initializes signed work as `denied` or
`deferred`. Admission, editor checks and subsequent transitions belong to the
worker and admission slices. Eligible work starts as `deferred` with
`awaiting_admission`; no projection is claimed by intake alone.

The worker must consider the stored delivery's event and action, including
withdrawals. Delivery deduplication does not replace revision admission or a
committed dispatch receipt. The worker, its transaction, and R1/R2/R3
reconciliation are separate changes (CHE-2208 and CHE-2209).

Keep the deployed `github_review_members` mapping unset until gate G. Numeric
ID `129714106` is not mapped by this change. No actionable dispatch, App
subscription update, webhook replay or deployment is enabled here.

## Recovery window

GitHub documents delivery history for the past **3 days**:
https://docs.github.com/en/webhooks/testing-and-troubleshooting-webhooks/viewing-webhook-deliveries

This is a documented visibility window, not a longer-term retention guarantee.
R2 must reconcile within that window. Missing revisions without a stored
signed delivery follow R3 (`unsigned`, exact-snapshot approval required),
never automatic admission from API text alone.
