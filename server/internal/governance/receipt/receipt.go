// Package receipt is the observe-only capture hook for Jev governance
// (CHE-685). It runs the pure server/internal/governance evaluator against a
// caller-built [governance.Input] and persists the outcome as a
// governance_receipt row (migration 504) — a "routing receipt" — WITHOUT
// ever coupling that capture to, blocking, or failing the comment/native
// mention-queue transaction it observes.
//
// This is D03: receipt infrastructure only. Nothing in this package, or any
// caller of it, ever acts on a captured Decision — no mention, correction,
// or status change is driven by a receipt. Live Jev provider wiring is a
// separate, still-blocked delivery; every Provider this package is given in
// production today is nil (see [Observer.Observe] and CHE-685's brief) and
// every Provider used in tests is a fake with no network access.
//
// Three properties make this safe to run inline on the request goroutine
// that just committed a comment:
//
//  1. A hard 50ms observation budget ([Budget]) around the
//     governance.Evaluate call, enforced with context.WithTimeout against a
//     context.Background()-derived context — never the caller's request
//     context, which may already be near cancellation by the time this runs
//     post-commit. The receipt persistence write that follows has its own,
//     more generous [writeBudget]: it is a separate best-effort DB call,
//     not part of the evaluation's own tight ceiling (see writeBudget's
//     doc).
//  2. A process-wide concurrency cap of exactly 1 ([Observer.Observe]):
//     a second concurrent observation attempt is shed immediately
//     (ReasonPoolBusy) rather than queued, so receipt observation can never
//     pile up unboundedly under load. See the package doc on that choice.
//  3. No synchronous retry. A shed or errored observation is terminal for
//     that comment — this package never schedules a backfill or
//     recreation attempt. A future delivery may add one; this one must not,
//     per CHE-685's explicit scope.
package receipt

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/governance"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Budget bounds the governance.Evaluate call: provider round trip plus
// local decision logic. CHE-685 fixes this at 50ms — tight enough that a
// wedged fake/live provider can never meaningfully delay the request
// goroutine it shares a process with.
//
// Budget deliberately does NOT also bound the receipt persistence write
// (see [writeBudget]): the issue's storage-isolation requirement asks for
// the write to happen in "a SEPARATE, best-effort post-commit
// transaction/call", not for it to share Evaluate's own tight ceiling.
// Collapsing both into one 50ms window made an otherwise-successful
// evaluation record shed_reason=budget_exceeded purely because of ordinary
// connection-pool cold-start latency on the write — exactly the kind of
// slow-but-real receipt this table exists to keep visible, not discard.
const Budget = 50 * time.Millisecond

// writeBudget bounds the persistence write independently of Budget. It is
// generous relative to Budget because a connection-pool cold start or a
// momentarily busy database is a completely different failure mode from a
// wedged Jev provider, and conflating them punished the write for the
// evaluation's own tight ceiling. It still exists — this write must never
// block the caller indefinitely — but a receipt row that took 500ms to
// land because of transient DB latency is a receipt this delivery still
// wants recorded, not one it should discard as "too slow to count".
const writeBudget = 500 * time.Millisecond

// ShedReason names why an observation never reached a Decision, or reached
// one this package could not accept. Every value is recorded distinctly on
// the persisted receipt (governance_receipt.shed_reason) so a dashboard can
// separate "we were too busy" from "we tried and it broke" from "we tried
// and it was slow".
type ShedReason string

const (
	// ReasonPoolBusy: the process-wide cap-1 admission gate was already
	// held by another observation when this one was requested. This
	// observation was never started at all — Evaluate was never called.
	ReasonPoolBusy ShedReason = "pool_busy"
	// ReasonBudgetExceeded: the observation was admitted but did not
	// finish (provider call, decision logic, or the persistence write)
	// before Budget elapsed.
	ReasonBudgetExceeded ShedReason = "budget_exceeded"
	// ReasonError: the observation was admitted but never reached a
	// governance.Decision at all — no Provider configured, or the
	// evaluate goroutine panicked. (A provider error INSIDE
	// governance.Evaluate is not this: Evaluate's own contract turns a
	// provider error into a ReasonProviderError abstention, which is
	// still a fully-formed Decision and therefore a 'decided' receipt —
	// see [decisionResult].)
	ReasonError ShedReason = "error"
)

// Trigger names which comment-handler call site produced this observation.
type Trigger string

const (
	TriggerCreate Trigger = "create"
	TriggerEdit   Trigger = "edit"
)

// Store persists a governance receipt. Implemented by *db.Queries in
// production; tests may substitute a fake to assert on write failures
// without needing a real DB error to occur.
type Store interface {
	InsertGovernanceReceipt(ctx context.Context, arg db.InsertGovernanceReceiptParams) (db.GovernanceReceipt, error)
}

// Input is one observation request: everything [Observer.Observe] needs to
// run governance.Evaluate and persist the outcome. WorkspaceID, IssueID, and
// CommentID are the receipt's owner-scoping key; Eval is passed through to
// governance.Evaluate unchanged.
type Input struct {
	WorkspaceID pgtype.UUID
	IssueID     pgtype.UUID
	CommentID   pgtype.UUID
	Trigger     Trigger
	Eval        governance.Input
}

// Observer runs bounded, best-effort governance observations with a
// process-wide concurrency cap of exactly one.
//
// Why a plain cap-1 semaphore rather than a worker pool or a
// singleflight-by-key: receipt observation has no natural dedup key (every
// comment is a distinct observation, unlike e.g. the chat-quick-actions
// pass which dedups per chat session) and D03 has no throughput requirement
// beyond "don't let this pile up" — a bare atomic CompareAndSwap gate is the
// smallest correct implementation of that, and it is trivially safe for
// concurrent CreateComment/UpdateComment requests to share.
type Observer struct {
	// Provider evaluates one Jev request. A nil Provider means governance
	// observation is wired but has nothing to call — every Observe with a
	// nil Provider records ReasonError without attempting a call. Wiring
	// a live provider is out of scope for this delivery; every production
	// value here today is nil, and every test value is a fake.
	Provider governance.Provider
	// Store persists the receipt. A nil Store makes Observe a no-op
	// (nothing to observe into), used defensively so a caller that wires
	// the Observer but forgets Store fails safe rather than panicking on
	// the request path.
	Store Store

	busy atomic.Bool
}

// Result is what Observe reports back to its caller, for logging only — the
// caller (comment.go's post-commit hook) must never branch product behavior
// on it.
type Result struct {
	// Status is "decided", "shed", or "error", mirroring the persisted
	// row's status column.
	Status string
	// ShedReason is set when Status is "shed" or "error".
	ShedReason ShedReason
	// Decision is set when Status is "decided" — the evaluator's full
	// answer, for logging/tests. Never consulted to change behavior.
	Decision *governance.Decision
	// PersistErr is set when the receipt row itself failed to write. This
	// is reported for logging only — see the package doc: a persistence
	// failure never propagates to the caller as an actionable error.
	PersistErr error
}

// Observe runs one bounded, best-effort observation and persists its
// outcome. It NEVER returns an error to the caller: every exit path (pool
// busy, budget exceeded, provider error, decided, persistence failure)
// returns a Result for logging only. The evaluation itself never blocks
// past Budget; the persistence write is independently bounded by
// [writeBudget] (see its doc for why the two are not the same window).
//
// ctx is accepted for interface symmetry with the rest of this codebase's
// context-threading convention, but Observe deliberately builds its own
// context.WithTimeout(context.Background(), ...) windows rather than
// deriving from it: ctx is normally an HTTP request context that
// CreateComment/UpdateComment already returned a response on (or is about
// to) by the time this best-effort hook runs, and a caller-cancelled
// request must not be able to cut short a receipt write that the comment
// author will never see anyway — the two lifecycles are independent by
// design (see comment.go's post-commit call site).
func (o *Observer) Observe(_ context.Context, in Input) Result {
	if o == nil || o.Store == nil {
		return Result{Status: "error", ShedReason: ReasonError}
	}

	if !o.busy.CompareAndSwap(false, true) {
		result := Result{Status: "shed", ShedReason: ReasonPoolBusy}
		o.recordBounded(in, result)
		return result
	}
	defer o.busy.Store(false)

	ctx, cancel := context.WithTimeout(context.Background(), Budget)
	defer cancel()

	result := o.evaluate(ctx, in)
	// record uses its own independent writeBudget window rather than the
	// (possibly already-expired) evaluate ctx: a budget_exceeded result
	// must still get a real chance to write its row — reusing an expired
	// ctx here would make the INSERT fail immediately on every
	// slow-provider case, which is exactly the case this receipt exists to
	// make visible.
	o.recordBounded(in, result)
	return result
}

// recordBounded wraps record with its own fresh writeBudget-bounded
// context, so the persistence write is never handed an already-expired
// context from an evaluation that ran out of its own, tighter Budget.
func (o *Observer) recordBounded(in Input, result Result) {
	ctx, cancel := context.WithTimeout(context.Background(), writeBudget)
	defer cancel()
	o.record(ctx, in, result)
}

// evaluate runs governance.Evaluate and classifies its outcome into a
// Result, without touching storage.
func (o *Observer) evaluate(ctx context.Context, in Input) Result {
	if o.Provider == nil {
		return Result{Status: "error", ShedReason: ReasonError}
	}

	type outcome struct {
		decision governance.Decision
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			// Panic containment: Evaluate runs on its own goroutine here
			// so a slow OR panicking fake/live provider can never take
			// down the caller's goroutine, and so the select below can
			// enforce Budget even against a provider that ignores ctx
			// cancellation. An unrecovered panic on a bare goroutine
			// would otherwise crash the whole process.
			if rec := recover(); rec != nil {
				done <- outcome{err: errPanic(rec)}
			}
		}()
		decision, err := governance.Evaluate(ctx, o.Provider, in.Eval)
		done <- outcome{decision: decision, err: err}
	}()

	select {
	case <-ctx.Done():
		return Result{Status: "shed", ShedReason: ReasonBudgetExceeded}
	case out := <-done:
		return decisionResult(out.decision, out.err)
	}
}

// decisionResult classifies a completed governance.Evaluate call. Per
// governance.Evaluate's own contract, a non-nil err is only ever returned
// alongside a ReasonProviderError or ReasonInvalidResponse abstention — both
// of which are a fully-formed Decision, so that case is still a 'decided'
// receipt (the evaluator DID reach a terminal judgment, "abstain, and here
// is exactly why"), not a shed/error one. A Decision with neither an Action
// nor AbstainReason set only occurs when evaluate's own recover() path fed
// this the zero Decision alongside a panic error — never something
// Evaluate itself can produce — so that combination alone maps to 'error'.
func decisionResult(decision governance.Decision, err error) Result {
	if err != nil && decision.AbstainReason == governance.ReasonNone && decision.Action == nil {
		return Result{Status: "error", ShedReason: ReasonError}
	}
	d := decision
	return Result{Status: "decided", Decision: &d}
}

// errPanic normalizes a recovered panic value into an error for logging.
func errPanic(rec any) error {
	if err, ok := rec.(error); ok {
		return err
	}
	return errors.New("governance receipt: observation panicked")
}

// record persists the receipt row. Any failure is logged and swallowed —
// this is the "storage failure never fails the request" guarantee, enforced
// here at the one call site that ever writes a receipt row, rather than
// trusted to every future caller of Observe to remember.
func (o *Observer) record(ctx context.Context, in Input, result Result) {
	params := db.InsertGovernanceReceiptParams{
		WorkspaceID: in.WorkspaceID,
		IssueID:     in.IssueID,
		CommentID:   in.CommentID,
		Trigger:     string(in.Trigger),
		Status:      result.Status,
		Answers:     []byte("[]"),
	}
	if result.ShedReason != "" {
		params.ShedReason = pgtype.Text{String: string(result.ShedReason), Valid: true}
	}
	if result.Status == "decided" && result.Decision != nil {
		params.ObservedAt = pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
		params.AbstainReason = string(result.Decision.AbstainReason)
		params.Answers = marshalAnswers(result.Decision.Answers)
		if result.Decision.Action != nil {
			params.ActionKind = pgtype.Text{String: string(result.Decision.Action.Kind), Valid: true}
		}
	}

	if _, err := o.Store.InsertGovernanceReceipt(ctx, params); err != nil {
		slog.Warn("governance receipt persist failed",
			"error", err,
			"issue_id", in.IssueID,
			"comment_id", in.CommentID,
			"status", result.Status,
		)
	}
}

// marshalAnswers encodes the evaluator's RecordedAnswer set as JSON,
// falling back to an empty array (never NULL — see migration 504's doc
// comment on why "no row" and "empty answers" must stay distinguishable)
// if marshaling somehow fails.
func marshalAnswers(answers []governance.RecordedAnswer) []byte {
	if len(answers) == 0 {
		return []byte("[]")
	}
	data, err := json.Marshal(answers)
	if err != nil {
		return []byte("[]")
	}
	return data
}
