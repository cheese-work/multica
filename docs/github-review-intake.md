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
  an initial disposition. Revision identity includes workspace, installation,
  repository, object kind/ID, event/action, body hash, sender/author IDs, review
  state and signed object timestamps. Different GUIDs for the same revision
  share one work item. Comment update timestamps distinguish same-body edits.
  Reviews carry only `submitted_at`, not `created_at` or `updated_at`. Review
  edits are distinguished by body hash, action, sender and review state, not
  edit time. Returning to an earlier edited body with the same identity reuses
  that row rather than creating a fresh revision. If that row is already denied,
  it stays denied. A changed review state creates a separate revision.

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

Workspace deletion removes that workspace's normalized revision work in the
existing pull-request teardown transaction. Other workspaces' revision rows
remain. Raw signed receipts have global delivery GUIDs, not workspace ownership,
and one delivery can serve multiple workspace bindings. The
workspace deletion manifest retains those shared receipts, including denied
receipts without a known workspace, rather than deleting another workspace's
signed recovery source. Intake retention is separate from workspace teardown.

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
withdrawals. Intake immediately denies pending revisions for a signed dismissal
or deletion, scoped to workspace, installation, repository and object kind/ID.
Dispatched rows remain intact; the projection worker owns any later notify-only
withdrawal note. Delivery deduplication does not replace revision admission or a
committed dispatch receipt. R1/R2/R3 reconciliation remains separate (CHE-2209).

Keep the deployed `github_review_members` mapping unset until gate G. Numeric
ID `129714106` is not mapped by this change. No actionable dispatch, App
subscription update, webhook replay or deployment is enabled here.

## Worker and retry state

The server starts a context-bound worker that drains up to 25 due deliveries
every 5 seconds. PostgreSQL row locks with `SKIP LOCKED` let multiple servers
claim different deliveries. Normalized revision rows are locked in workspace
and canonical delivery order, so different GUIDs for the same revision serialize
on the same work. Reads match the full normalized revision identity, including
event/action, sender/author, review state and signed timestamps, not body hash
alone. State writes address only that canonical revision.
The original signed payload supplies the content; the worker does not fetch
replacement content from GitHub.

Before calling admission, the worker denies older pending revisions as
`superseded` when a newer signed self-edit for the same workspace, installation,
repository, object and author is stored. An edit at the initial revision's
timestamp also supersedes that initial revision. Foreign edits do not supersede
the author's pending work. Denied and dispatched history remains intact.

The production admission function currently rechecks the intake guards and
leaves eligible items `deferred` with `awaiting_admission`. Full provenance
admission (CHE-2214) and authorized notify-only comment projection (CHE-2212)
are separate slices. They must complete before production claims `projected`.
The worker accepts the six intake states from admission and stops at
`projected` when its internal dispatch gate is closed. No production caller
opens that gate, and no setting or environment variable enables it.

`attempts`, `next_attempt_at`, `received_at`, state and reason expose retry
progress without retaining error messages or third-party text. A deferred pass
backs off from 5 seconds to a maximum of 300 seconds, with no retry-count limit.
`ListDeferredGitHubWebhookIntakes` includes `age_seconds` for the subsequent
stale-item monitor. An admission or processing error rolls back its savepoint
before scheduling `processing_error`. A database commit failure rolls back
the entire pass and leaves the signed intake available for another worker.

When one delivery spans workspaces, its aggregate state prioritizes `deferred`,
`quarantined`, `unsigned`, `projected`, `dispatched`, then `denied`. Each
workspace's normalized revision retains its own state and reason. Duplicate
GUIDs retain separate delivery dispositions but share revision work.

The dormant dispatch primitive writes the system comment, queued task,
`github_webhook_dispatch` receipt and final revision/delivery state in one
transaction. Receipts are keyed by workspace, object kind, object ID, body
hash, canonical revision source GUID and target issue. Redeliveries select the
original canonical revision; a different redelivery GUID does not create another
receipt. Same-body edits retain separate receipts. A receipt is inserted only
when its matching canonical revision, task and trigger comment exist. Notifications publish only after
commit. A failed or interrupted transaction cannot commit a receipt alone;
a retry reads the committed receipt before creating another dispatch.
Workspace teardown removes that workspace's dispatch receipts with its
normalized revisions and preserves the shared signed delivery source.

Worker tests simulate an admission/API outage, restart after intake commit,
commit failure, concurrent duplicate deliveries and the closed gate. Tests
exercise the dormant transaction with a test-only open gate and injected
admission. They do not qualify gate G or replace the real GitHub provenance,
project authorization, projection, runner or deployment acceptance tests.

## Recovery window

GitHub documents delivery history for the past **3 days**:
https://docs.github.com/en/webhooks/testing-and-troubleshooting-webhooks/viewing-webhook-deliveries

This is a documented visibility window, not a longer-term retention guarantee.
R2 must reconcile within that window. Missing revisions without a stored
signed delivery follow R3 (`unsigned`, exact-snapshot approval required),
never automatic admission from API text alone.

## Normalization and dispatch-time admission

Content always comes from the stored HMAC-verified delivery, never a new API
response. Each revision retains its delivery GUID, event/action, object kind/ID,
created/updated/submitted timestamps, body SHA-256, PR head SHA, sender/author IDs
and review state. The raw receipt also retains the signed `node_id` and body.
An empty or null review body hashes the empty string. Inline comments are
independent signed deliveries, not fetched to fill an empty review body.

`ghsnapshot.ReviewEventKind` is the normalization table used by intake and
admission. All other event/action pairs are denied.

| Event | Initial | Edit | Withdrawal | Object kind |
| --- | --- | --- | --- | --- |
| `pull_request_review` | `submitted` | `edited` | `dismissed` | `review` |
| `pull_request_review_comment` | `created` | `edited` | `deleted` | `review_comment` |
| `issue_comment` on a PR | `created` | `edited` | `deleted` | `issue_comment` |

The dispatch worker calls `Manager.CheckReviewRevision` with a `ReviewRevision`
constructed from the stored signed receipt and the currently authorized numeric
author ID. This method does not cache provenance, persist an admission or dispatch
a run. A positive result is `admitted/verified_revision`, not a database state or
permission to bypass gate G, project authorization or screening. Intake continues
to defer eligible revisions as `awaiting_admission`; this slice does not install
or enable the dispatch worker.

Each call reads the signed node through installation-authenticated GraphQL. The
node ID, full numeric database ID and object type must match. The API body is
used only for a SHA-256 equality check and is never returned as actionable text.

Permanent intake denials, including fork PRs and missing issue linkage, take
precedence over foreign-editor quarantine. Only an otherwise eligible revision
can be quarantined for a foreign signed sender.

| Signed revision and GraphQL provenance | Decision |
| --- | --- |
| Initial: sender = author = trusted ID; `editor: null`, `lastEditedAt: null`, equal body hash | admitted |
| Initial review: same checks, signed state `approved`/`commented`/`changes_requested`, unchanged GraphQL state | admitted, including empty body |
| Self-edit: sender = author = trusted ID; GraphQL editor is that `User`, equal body hash and unchanged review state | admitted as a separate revision |
| Foreign sender or editor | quarantined |
| Edit provenance unknown, including edited timestamp with null/non-User editor | quarantined |
| Initial revision now edited, changed body hash or changed review state | quarantined |
| Dismissal/deletion | denied; pending revisions denied atomically at intake |
| Missing/untrusted author or unsupported review state | denied |
| Lookup error, timeout, missing fields, wrong/missing node or malformed response | deferred |

`updated_at` alone is not an edit signal. Initial review creation, submission and
update timestamps may differ. GraphQL review-state enums are compared with their
signed lowercase equivalents. No GraphQL author login is used for trust.

Tests cover T3, T4, T13, T14, T15, T22, T23, T24 and T25 in
`admission_test.go` and `github_admission_test.go`. Dispatch exactly-once receipts
and replacement of pending self-edits are worker duties (CHE-2208), not side
effects of this admission check. Withdrawal projection remains a separate
CHE-2209 duty.

Migrations 728/729 replace body-only uniqueness with revision identity using
concurrent indexes. Rolling back 729 can fail safely if newer revisions share a
body hash; no rollback deletes signed receipts or revisions to force the old
constraint. Resolve that data-preservation decision before retrying rollback.
