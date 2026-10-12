# Decision transaction primitives

This package is the schema/transaction foundation for the revision 14 decision
contract. It does not implement intent, lane, request, answer or worker routes.

`Begin` acquires the existing workspace `FOR KEY SHARE` fence before returning
a transaction. A missing workspace or failed fence rolls back and returns no
transaction. The caller owns commit/rollback after success and constructs
`db.New(tx)` for the scoped decision queries.

The caller must follow the contract's remaining lock order: at most one session,
first after the workspace; owner fences if needed; comment-thread advisory;
issue; NOWAIT task/member/source rows; intent; targets; request; answer. The
queries do not authorize a caller or validate its task status, assignee,
mentions, expiry or source authorship. Those checks belong to later slices.

Calling tasks use `FOR SHARE NOWAIT`, delivery tasks `FOR UPDATE NOWAIT`, asking
agents `FOR SHARE NOWAIT`, and members/source messages `FOR KEY SHARE NOWAIT`.
Task and source-message scope joins lock only the selected task/message, not
the joined agent/session. Source-message batches lock in ascending ID order.
Callers must reject missing source rows rather than treating a shorter batch
as complete.

`IsRetryable` recognizes only SQLSTATE `55P03` and the contract's named `40P01`
exception, including wrapped errors. It does not retry or return HTTP responses.
The caller applies the contract's bounded retries and retryable 503 policy;
constraint, authorization and unrelated connection failures are not classified
as lock retries.

`ValidateIntent` is the shared guard for synchronous window consumers. Call it
after the workspace and issue fences, any calling-task/member locks, and the
intent's `FOR UPDATE` lock, before making any other writes. It reads the tag,
task and unconsumed targets and compares expiry with `clock_timestamp()`.
An invalid window is sealed (and a missing or tombstoned tag closes it) and
committed before `ErrIntentClosed` is returned. This refusal ends the transaction;
rolling it back afterwards cannot discard the seal. Other errors do not commit.
A valid window leaves the transaction open for its consumer. The lane/request
slices wire this guard into their own operations; baseline comment deletion
continues to run no decision statement.

All decision IDs referencing baseline rows are plain UUIDs. The task-specific
approval permits exactly three internal cascading FKs: answer/request,
class-change/request and intent-target/intent. Baseline application transactions
do not write decision tables. Owner deletion cleanup remains the later worker's
responsibility.

The unexported handler `createDecisionRequest` is the transactional ping core.
It takes a loaded issue and a validated lane UUID with the ask/class strings;
it re-checks issue scope, assignment and terminal status under the issue fence.
Only middleware-stamped issue-task provenance is admitted. It locks the
workspace, one session, the ask thread, issue, NOWAIT task/member rows, intent
and target, in that order. The invalid-window guard runs before any ask, DM
or thread-resolution write, so refusal can persist only the window seal/close.

Success returns the committed request, stamped ask comment, non-turn DM message
and any cleared thread resolutions. The lane stays immutable. IDs for the
comment/message are allocated before the request insert, so all links commit
together without a separate request update. One target is consumed, and the
last target seals and consumes the intent. Failure returns an empty result.
Lock contention gets at most three attempts; the named deadlock gets at most
one retry. Retryable database errors remain recognizable to the later API's
503 mapper.

The core performs no post-commit side effect. The public request handler calls
it before the inbox consumer or any subscriber/mention/realtime event.
`POST /api/decisions` accepts `issue_id`, `session_id`, `ask`, and
`escalation_class`. Only the running issue-task provenance admitted by the core
can request a decision. `GET /api/decisions/{id}` and
`GET /api/decisions?issue=<UUID-or-identifier>` require workspace membership and
an existing issue in that workspace. They expose request metadata, never DM
transcripts. An empty list is `[]`.

The CLI provides `multica decision request --issue <id> --session <id>
--class <enum> --ask-file <path>`, `multica decision get <request-id>`, and
`multica decision list --issue <id>`. The ask file must be nonempty UTF-8;
whitespace and newlines are preserved. Each command supports JSON and table
output. A ping never enqueues a DM turn.

`decisiontx.NotifyPing` is shared by the request handler and the later decision
worker. It locks workspace, issue, then request. A NULL receipt is re-checked
under the request lock. The `mentioned` inbox item and `ping_notified_at`
commit in one transaction; cancellation writes only the receipt. Concurrent
consumers, repeated retries, or a failed receipt write cannot leave duplicate
or partially committed inbox items. The returned item is safe to publish only
after this function succeeds. Missing requests and already-received pings
return no item. A missing workspace/issue remains an error for later orphan
cleanup, not a successful receipt.

`ListDecisionPingRetries` is a bounded, ordered worker-facing selector for
asked requests with a NULL receipt, including cancelled requests. The public
handler returns 201 for a committed core even if the receipt transaction
fails; the NULL receipt keeps that ping eligible for worker retry. The worker
is supplied by CHE-1550, not this slice. Generic comment notifications suppress
the ping's named target via the existing decision-intent target exclusion;
other subscriber and mention notifications retain their existing behavior.

Migrations create parent tables first, then their concurrent unique ID indexes,
then the dependent tables. Each explicit index build/removal has its own
single-statement migration. The target's composite primary key attaches its
already-built concurrent unique index; it does not build another index. Each
concurrent build is registered with the migrator's invalid-index cleanup hook,
so an interrupted build is removed before retry instead of skipped as present. Reverse
order drops dependents before parent identity indexes. If an operator manually
removes a recorded prerequisite, restore that parent table/index from its
migration before retrying the dependent migration. Do not mark a missing
prerequisite as successfully applied.

Comment stamps are nullable server-side insert parameters. Ordinary callers
leave them unset; no HTTP request field or decision command is added here.
Migration 596 adds the comment columns without inline checks and installs the
kind constraint `NOT VALID` with a transaction-local 2-second lock timeout.
Migration 612 validates it in a separate transaction, so the validation scan
does not hold the column-addition lock. New writes are checked before validation.
An already-applied 596 retains its existing constraint; 612 can validate it
without replaying 596. Validation rollback is a no-op; 596 rollback drops the
columns and their constraint. A lock-timeout failure leaves the migration
unrecorded; retry after the conflicting transaction ends.
Edits preserve stamps without reassigning them. Existing message provenance
(`sent_via`, `message_kind`) is inherited unchanged from the predecessor; the
three decision message kinds fit the existing unconstrained text column.
