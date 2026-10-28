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

The webhook endpoint creates no issue comment, agent mention or run. The
worker projects only a provenance-admitted revision whose mapped member
passes `can_route` for that issue. Its system comment contains the canonical
GitHub permalink, author login and numeric ID, event type, object ID and
intake ID. Review bodies, inline text, filenames and supplied URLs are not
copied into issue history. The former synchronous CHE-2019 path stays dormant.
An `issue_comment` delivery does not contain a PR head object. That event
uses the non-fork repository verification already mirrored from signed PR
events. Missing or unverified stored metadata does not authorize projection.

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
committed dispatch receipt. R1/R2/R3 reconciliation is described under
"Reconciler (R1/R2/R3)" below.

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
repository, object and author is stored. For reviews, any stored self-edit
supersedes the submitted revision, even when the submission arrives later.
Successive edited review bodies are ordered by the canonical revision's
server-assigned `created_at` receipt time, with its source GUID breaking ties.
This is receipt order, not proof of GitHub edit chronology; authoritative
provenance admission remains required before production projection or dispatch.
Comments use their signed `updated_at` first, then receipt order for equal
update timestamps. Foreign edits do not supersede the author's pending work.
Denied and dispatched history remains intact.
Redelivery of a dispatched revision with a committed receipt reuses its stored
disposition without calling admission again. A dispatched state without a
receipt still requires admission and a committed dispatch transaction.

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

Projection and the dormant dispatch primitive write their effects,
`github_webhook_dispatch` receipt and final revision/delivery state in one
transaction. Receipts use the normalized revision UUID and target issue, not
the delivery GUID or body hash alone. The worker matches the full signed
event/action, sender, author, state and timestamps before locking a revision.
Distinct normalized revisions retain separate receipts; repeated review edits
with the same identity reuse the canonical revision and receipt.

The receipt records `projected` with a comment and no task, `denied` with no
comment or task, or `dispatched` with both. Each linked issue gets its own
authorization result. The worker evaluates `can_route` during projection
admission and again through transaction-bound queries immediately before
creating effects. It reloads the current issue/project and verifies the
installation, PR link and target agent. Revoked grants give `denied`; lookup
errors give `deferred`, not authorization. A denied issue does not prevent
projection on another granted issue. Transient failures remain retryable.

Notify-only comments carry the server-owned `github_notify_only` marker.
Database comment-event triggers skip marked inserts, updates and deletes,
so a `comment.created` subscription cannot turn a notification into a run.
Ordinary comments retain their wakeup behavior. The server does not expose
the marker as an input to ordinary comment APIs.

Notifications publish only after commit. A failed or interrupted transaction
cannot commit a receipt alone; a retry reads the committed receipt before
creating another effect. Production keeps the actionable gate closed.
Workspace teardown removes that workspace's dispatch receipts with its
normalized revisions and preserves the shared signed delivery source.

Worker tests simulate an admission/API outage, restart after intake commit,
commit failure, concurrent duplicate deliveries and the closed gate. Tests
exercise the dormant transaction with a test-only open gate and injected
admission. Projection tests exercise the signed HTTP intake through real
provenance admission, per-project authorization and the worker transaction.
They cover T10, T19 and T21, same-body self-edits, duplicate receipts,
lookup failures at both route checks and a normal-comment wakeup control.
These checks do not qualify gate G or deployment acceptance.

Migrations `748` through `752` add normalized revision IDs, route-result
receipts and the no-wakeup marker. An existing dispatch receipt must match
exactly one normalized revision. Migration `748` fails closed on an ambiguous
or missing match; an operator must reconcile that receipt before retrying.
Rollback preserves issue comments and signed intake, but removes notify-only
and denied receipts before restoring the older body-key index. Rollback after
actionable dispatch may need operator reconciliation if multiple dispatched
revisions share the former body-only key. No deployment or rollback is run
in production by this change.

## Recovery window

GitHub documents delivery history for the past **3 days**:
https://docs.github.com/en/webhooks/testing-and-troubleshooting-webhooks/viewing-webhook-deliveries

This is a documented visibility window, not a longer-term retention guarantee.
R2 must reconcile within that window. Missing revisions without a stored
signed delivery follow R3 (`unsigned`, exact-snapshot approval required),
never automatic admission from API text alone.

## Reconciler (R1/R2/R3)

`RunGitHubIntakeReconciler` runs every 10 minutes beside the intake sweep. It is
stateless: GitHub's delivery log caps attempts and rate-limits redelivery.

- **R1** retries a stored signed payload through the intake worker (CHE-2208).
- **R2** lists `GET /app/hook/deliveries` with the App JWT (cursor from the
  `Link` header, URL never followed, at most 20 pages; a log that is not fully read redelivers nothing) and asks
  `POST /app/hook/deliveries/{id}/attempts` for a failed delivery older than the
  10-minute grace and newer than 72 hours. The redelivery is signed by GitHub and re-enters the normal
  webhook path. It stops after 5 attempts per GUID.
  Off unless `GITHUB_INTAKE_REDELIVER=1`.
- **R3** reads open, same-repo pull requests through the REST API and records
  each review, inline comment or PR comment with no stored signed event as an
  `unsigned` revision. The body is hashed, never stored. Only the newest stored revision
  of the object is compared (body, review state and timestamps), so an edit back
  to an earlier body or a same-body edit is recorded. Insert is idempotent on
  the revision identity index; a repeat pass records nothing. `unsigned` is never dispatched automatically.
  Off unless `GITHUB_INTAKE_UNSIGNED=1`.
- A revision with no `source_delivery_guid` settles as `unsigned`
  (`no_signed_delivery`), not as a retrying error.
- **Alert:** a `deferred` or `unsigned` row older than 1 hour logs
  `slog.Error` with `alert=github_intake_stale` (item, ref, state, reason,
  `age_seconds`). Routing that log line to a pager is an operator step.

The approval half of R3 (exact-snapshot approval) lands in 5b. Notify-only until
gate G: no actionable dispatch, no webhook replay, no App subscription change.

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
effects of this admission check. Withdrawal projection is not part of the
reconciler.

Migrations 728/729 replace body-only uniqueness with revision identity using
concurrent indexes. Rolling back 729 can fail safely if newer revisions share a
body hash; no rollback deletes signed receipts or revisions to force the old
constraint. Resolve that data-preservation decision before retrying rollback.
