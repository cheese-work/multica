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
Edits preserve stamps without reassigning them. Existing message provenance
(`sent_via`, `message_kind`) is inherited unchanged from the predecessor; the
three decision message kinds fit the existing unconstrained text column.
