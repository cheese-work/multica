# Transaction-scoped concurrency (C18)

`LockConcurrencyResources(ctx, tx, workspaceID, resources...)` takes the
workspace configuration/control-epoch row lock, then creates and locks stable
resource guards in sorted, deduplicated resource order. Initial rows use
`INSERT ... ON CONFLICT DO NOTHING` followed by a locked reread. Call once with
every resource needed by the transaction, before acquiring any downstream locks.

The caller owns the transaction, workspace authorization/existence checks and
resource-specific cap from the locked configuration. `Reserve` accepts that
trusted cap and expected control epoch; it is not a provider or queue admission
API. The boolean return is `duplicate`: `false, nil` creates a hold;
`true, nil` reuses an existing held reservation. Missing/disabled configuration
or a stale epoch refuses admission. A zero or reduced cap refuses new holds
without deleting existing holds. A released reservation cannot reacquire.

`Release` returns whether it changed a hold from held to released. It remains
available with governance disabled so settlement can complete. Repeated or
unknown releases return `false, nil` and never decrement the counter. Released
rows are retained as idempotency tombstones until explicit workspace teardown.

Neither primitive begins, commits or rolls back a transaction. On any failure,
the caller rolls back **all** effects, including holds/counters, original-window
spend accounts, budget root, attempt and outbox. C16 owns that integration and
trusted terminal/reconciliation evidence before calling `Release`.

Lock order: configuration/control epoch → stable resource guards (sorted) →
billing-window spend accounts → budget root → issue/thread → receipt → case →
attempt → action/obligation. Hold identity has no billing window, case generation
or lease; queued, running and unknown execution keeps its slot until explicit
release. Window rollover and timestamps never release a hold.

This slice installs persistence and internal primitives only: no runtime
callsite, provider use, queue admission or flag activation. Existing default-off
comment/observation paths execute no additional query. C15/C16 own integrated
rollover/spend/queue proof; observation latency, audit pagination and shadow
promotion remain with their owning slices. The focused DB suite retains
`EXPLAIN (ANALYZE, BUFFERS)` for both workspace-leading identity indexes.

Tests: source this checkout's `.env.worktree`, apply migrations using
`go -C server run ./cmd/migrate up`, then run
`bash scripts/go-test-with-agent-cli-guard.sh -- go -C server test -race -v ./internal/governance -run TestConcurrency`.
