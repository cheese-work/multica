# Decision transaction primitives

This package is the schema/transaction foundation for the revision 14 decision
contract. HTTP routes live in the handler package; the worker is a later slice.

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

## CHE-1630 worker-callable consequences

`handler.CheckDecisionSource(ctx, workspaceID, requestID)` runs one source-check
transaction. It locates the request with a plain read, then locks workspace,
the current lane, the ask thread, issue, request and live answer in that order.
It rechecks the located context and source existence under those locks. Missing
sources invoke `Withdraw` with `source_removed`, including immutable snapshots,
cancelled-request preservation and archived/unprompted cancellation receipts.
Source edits alone do not withdraw an answer. Already-withdrawn answers, missing
owners and intact sources are no-ops. Its committed `Withdrawal` returns comments
and cleared resolutions for the worker's post-commit publication; errors return
no uncommitted objects.

Use `len(result.Comments) > 0` as the source-check action signal. An intact
source can return the current request and answer with empty `Comments`; nonzero
request or answer IDs do not signal a withdrawal. A settled retry returns no
comments or cleared resolutions to publish again.

`handler.SealDecisionIntent(ctx, workspaceID, intentID)` locks workspace, issue
and intent, then rechecks the tag, terminal/missing bound task and expiry. It
seals invalid windows and closes missing/tombstoned tags with `comment_deleted`.
`SealInvalidIntent` is the shared, non-committing transition used by both this
transaction and `ValidateIntent`. Callers of the transition must already hold
the workspace, issue and intent fences. Live windows and sealed retries leave
timestamps and unconsumed targets unchanged.

`ListDecisionSourceChecks(ctx, batchSize)` returns scoped request rows;
`ListDecisionSealIntents(ctx, batchSize)` returns intent rows. Both are plain
reads, deterministically ordered by ID and capped at 100 rows. Non-positive
limits return no rows. Neither selector takes a decision lock. Missing baseline
owners are left to CHE-1550's orphan cleanup, not repeatedly selected. Selected
rows are hints: each transaction independently rechecks eligibility.

CHE-1550 owns the lease, scheduling, retries, post-commit events, Finalize,
Lifecycle cancel and deferred deliveries. These methods do not notify, enqueue,
deliver notes or install baseline hooks. SQLSTATE lock/deadlock errors remain
available to `decisiontx.IsRetryable`; the caller owns bounded retries. The
runtime-unbind/detach ordering and the merge `trigger_comment_id` FK corrections
remain unchanged in the record/veto code comments; no protocol change is made.

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

CHE-1549 adds the private asked-record core in the handler package. It resolves
the nearest stamped parent before filtering status, then locks workspace,
session, thread, issue, calling task (`FOR SHARE NOWAIT`), and request. Under
those locks it re-checks the running chat task, asked/open request, human and
DM lane, and interactive human source messages after the ping and before the
turn started. Snapshot, stamped quote, DM reference, and answered state commit
together; failure leaves no partial result. One source may answer several
requests, but a second answer to the same request is refused. `wake_at` stays
NULL. The database clock sets the effective time, with the ten-minute hold
for non-reversible classes. Lock contention has three attempts; a deadlock has
at most one retry. The core has no post-commit effects.

Both archive paths read the open-lane guard after locking the session and
before continuation side effects. Hard delete refuses open lanes and messages
referenced by any answer, including withdrawn answers, and identifies the
referencing issues. These baseline paths use plain decision reads only;
comment deletion and task completion do not write a decision consequence.
Public record CLI/API and unprompted records are deferred to CHE-1628, with
worker finalization, delivery and source checks invoked by CHE-1550.

The request core performs no post-commit side effect. The public request handler calls
it before the inbox consumer or any subscriber/mention/realtime event.
`POST /api/decisions` accepts `issue_id`, `session_id`, `ask`, and
`escalation_class`. Only the running issue-task provenance admitted by the core
can request a decision. `GET /api/decisions/{id}` and
`GET /api/decisions?issue=<UUID-or-identifier>` require workspace membership and
an existing issue in that workspace. They expose request metadata, never DM
transcripts. An empty list is `[]`.
The ask shares the existing 64 KiB comment-content limit. JSON decoding is
bounded using the existing eight-times-content allowance for escaped input.
The ping touches the held session's activity timestamp inside the core
transaction, so DM ordering and default-lane selection reflect the message.

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
Migration 597 adds the comment columns without inline checks and installs the
kind constraint `NOT VALID` with a transaction-local 2-second lock timeout.
Migration 613 validates it in a separate transaction, so the validation scan
does not hold the column-addition lock. New writes are checked before validation.
An already-applied 597 retains its existing constraint; 613 can validate it
without replaying 597. Validation rollback is a no-op; 597 rollback drops the
columns and their constraint. A lock-timeout failure leaves the migration
unrecorded; retry after the conflicting transaction ends.
Edits preserve stamps without reassigning them. Existing message provenance
(`sent_via`, `message_kind`) is inherited unchanged from the predecessor; the
three decision message kinds fit the existing unconstrained text column.

Migration 612 rollback drops the primary key in a transaction-local 2-second
lock-timeout hook, then recreates its original unique index concurrently in the
single-statement down file. The hook also removes an invalid index from an
interrupted rebuild before retrying. Bounded rollback keeps its named target
applied: `down --to 612_decision_target_primary_key` retains the primary key;
`down --to 611_decision_answer_request_index` exercises the 612 rollback.

CHE-1628 wires `issue comment add --decision-record --message <id>` to the
record transaction. No stamped parent means an unprompted request and answer
commit together; an ordinary parent controls placement without becoming an
ask. Unprompted records require an explicit class and an active member on an
eligible same-workspace issue. When unsure of the class, the DM agent must
choose a held class, never `reversible`. A pending quote never triggers a task.
The DM reference includes the class, issue link and answer-specific disclaim
endpoint. Wake finalization and worker notification remain the worker slice.

`POST /api/decision-answers/{id}/disclaim` accepts only the request's human.
The handler fences workspace, current session, ask thread and issue, then
locks request and answer. It re-checks the immutable request context and reads
archive state under the session fence. It takes no task lock and never
rebinds, creates a DM, re-pings or delivers a re-ask itself.
An already-withdrawn answer returns 409 from this human endpoint. The worker
helper remains idempotent. A physically deleted anchor needs no thread lock;
the withdrawal comments are top-level instead. Server-written disclaim and
cancel comments use the asking agent; `disclaimed_by_user_id` retains the human.

`Withdraw` is the shared human/worker transition. Call it with transactional
queries and freshly loaded request/answer rows after those same locks and any
caller-specific authorization. The caller commits or rolls back all results
together and publishes comments only after commit. The helper never opens its
own transaction. It preserves source IDs, snapshot, quote and prior cancelled
reason. A repeated withdrawal is a no-op. A live asked answer reopens only in
an active lane; an archived lane cancels with `lane_archived`, a cancel comment
and a durable notice receipt for the later worker's re-ask. Unprompted answers
cancel with `record_disclaimed`; their disclaim comment is the cancellation
notice, and no re-ask becomes due. Every withdrawal writes its disclaim comment
and receipts atomically. Pending wakes are cleared; delivered wakes retain
their history so the worker can select the withdrawal for retraction.

CHE-1629 adds `multica issue comment add --decision-veto --parent <quote-id>
--content-file <reason>`. Only a running assigned issue task (including an
assignee squad member) or a running chat task of the immutable source DM can
withdraw a stamped answer. The transaction locks workspace, current session,
quote thread, issue, NOWAIT calling task, request and answer, in that order.
It rechecks the stamp and task under those fences. Task contention gets three
attempts; a deadlock gets only one retry. Exhaustion returns 503 with
`Retry-After: 1` and commits nothing.

`WithdrawByAgent` reuses the human/worker withdrawal transition. A veto comment
retains the full reason; `veto_reason` retains its first 600 Unicode characters.
Agent and task attribution are separate from human attribution. Asked answers
reopen only in a live lane; cancelled requests preserve their reason; archived
lanes cancel with a notice; unprompted records cancel with `record_vetoed`.
Only an issue-agent veto by the current retraction recipient (agent assignee
or squad leader) acknowledges an already-delivered wake as
`suppressed_agent_veto`. A non-leader squad member and a source-DM task never
suppress that retraction. DM notes, inbox delivery, worker finalization and
retraction delivery remain CHE-1550; these handlers do not invoke baseline
comment triggers or enqueue work.

`POST /api/decision-requests/{id}/class` accepts only the request's human.
Raising means changing `reversible` to one of the five escalation classes;
those classes are unranked, not an invented severity hierarchy. Same-class,
sideways and lowering changes return 409, as does any cancelled request.
Workspace, request and live-answer fences serialize class changes with record,
withdrawal and worker operations. The class update and `decision_class_change`
audit commit together. A live, un-woken answer gets a fresh ten-minute hold;
an existing schedule is recomputed to that same time, but a pending answer's
NULL `wake_at` remains NULL. Withdrawn and already-woken answers never move.

The scoped regression inventory spans guards, intent/ping, asked/unprompted
recording, human withdrawal, agent veto and raise-class integration. Run from
`server/` with the checkout's isolated `DATABASE_URL`:

```sh
go test -race -p 1 ./internal/handler ./internal/decisiontx ./cmd/server -run 'Decision|AskedDecision|UnpromptedDecision' -count=1
go test ./cmd/multica -run 'Decision' -count=1
```
