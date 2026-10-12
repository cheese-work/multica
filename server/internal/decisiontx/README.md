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
