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
//     post-commit. Observe returns to its caller as soon as Evaluate
//     produces a Result: the request goroutine never makes the receipt
//     persistence DB call itself (see writeQueue's doc below) — Budget is
//     the caller's entire latency exposure, full stop.
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
	"sync"
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
// Budget bounds ONLY the evaluation. The persistence write is never part of
// this window: it happens on a separate, supervised background goroutine
// (see writeQueue) after Observe has already returned to its caller. An
// earlier version of this package gave the write its own second,
// request-goroutine-blocking deadline (writeBudget); that made the request
// pay for however long the write took (see CHE-685 review B2) and made the
// receipt's own tests flake under ordinary DB contention (see review B1).
// Moving the write off the request path entirely removes both problems at
// once: Budget is now the caller's ENTIRE latency exposure, and the write's
// own deadline (DefaultWriteBudget / Observer.WriteBudget) can stay generous
// without that generosity ever reaching the request.
const Budget = 50 * time.Millisecond

// DefaultWriteBudget bounds the background persistence write when
// Observer.WriteBudget is left unset. It is generous relative to Budget
// because a connection-pool cold start or a momentarily busy database is a
// completely different failure mode from a wedged Jev provider, and this
// write never blocks the request goroutine that produced it — only the
// background writer goroutine, which has nothing else to do while it waits.
// A receipt row that took a couple seconds to land because of transient DB
// latency is still a receipt this delivery wants recorded, not one it
// should discard as "too slow to count".
//
// CHE-685 review B1 originally flagged 500ms as too tight: under this
// repo's own handler test suite (many sequential DB-heavy tests sharing one
// small pgxpool, plus this package's own background writer competing for a
// connection alongside them), a 500ms deadline measurably timed out even
// though nothing was actually wrong — the DB was simply momentarily busy.
// 5s matches the single-best-effort-DB-call convention already used
// elsewhere in this codebase for a similar off-request-path write (see
// e.g. internal/scheduler/manager.go's heartbeat write and
// internal/handler/github.go's webhook persistence, both 5*time.Second) —
// generous enough to absorb ordinary contention, while still being a
// deliberate, bounded ceiling rather than no deadline at all. Because this
// write no longer touches the request path in any way (see Budget's doc),
// there is no latency-budget tension with raising it: the only cost of a
// slower deadline here is a slower background writer, not a slower caller.
const DefaultWriteBudget = 5 * time.Second

// writeQueueCapacity bounds the background writer's buffered channel. This
// is the "supervised, bounded writer" CHE-685 review B2 asked for in place
// of a synchronous request-path write: a small, fixed backlog so a
// pathologically slow database can never grow unbounded memory here, paired
// with cap-1 admission (busy) upstream, which already limits how fast this
// channel can fill — in steady state at most one item is ever in flight
// between an Observe call finishing evaluation and the writer picking it up.
// The value is generous relative to that steady-state rate purely as slack
// for a slow writer goroutine draining a burst of near-simultaneous
// evaluations across process restarts of the cap; it is not sized to any
// throughput target, since D03 has none (see Observer's doc).
const writeQueueCapacity = 32

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
	// finish evaluating (provider call or decision logic) before Budget
	// elapsed. The persistence write is never part of this window (see
	// Budget's doc) — a budget_exceeded observation still gets its shed
	// outcome queued for the background writer like every other outcome.
	ReasonBudgetExceeded ShedReason = "budget_exceeded"
	// ReasonError: the observation was admitted but never reached a
	// governance.Decision at all — no Provider configured, or the
	// evaluate goroutine panicked. (A provider error INSIDE
	// governance.Evaluate is not this: Evaluate's own contract turns a
	// provider error into a ReasonProviderError abstention, which is
	// still a fully-formed Decision and therefore a 'decided' receipt —
	// see [decisionResult].)
	ReasonError ShedReason = "error"
	// ReasonWriteQueueFull: the observation reached a terminal outcome
	// (decided, shed, or error) but the background writer's queue was
	// already at writeQueueCapacity when this result tried to enqueue.
	// The result itself is dropped rather than blocking Observe's caller —
	// per the package doc, Observe must never add synchronous DB latency to
	// the request path, and that guarantee has to hold even when the
	// writer is falling behind. This is a Stats-only counter (see
	// Observer.Stats): no governance_receipt row exists for a
	// write-queue-full outcome, because there was nowhere bounded to
	// record it without violating the guarantee it exists to protect.
	ReasonWriteQueueFull ShedReason = "write_queue_full"
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
//
// Persistence lives on a separate, lazily-started background goroutine (see
// writeQueue / startWriter), not on the request goroutine that called
// Observe: CHE-685 review B2 flagged that a synchronous write after
// evaluation could add up to writeBudget's full deadline to the request
// path, on top of Budget's own 50ms, including on the pool_busy shed path.
// Moving the write off-path means Observe's only caller-visible cost is
// Budget, always — evaluation timing does not change based on how the write
// eventually goes. See recordAsync's doc for the queue-full fallback.
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
	// WriteBudget bounds each background persistence write independently
	// of Budget. Zero (the default production value) falls back to
	// DefaultWriteBudget. Tests set this explicitly instead of relying on
	// ambient DB speed: a generous value proves a receipt row lands
	// deterministically; a near-zero value proves the write times out
	// deterministically — see receipt_test.go's write-budget cases. It is
	// read once per write by the background writer goroutine, so changing
	// it after the writer has started only affects writes queued after the
	// change is visible to that goroutine (tests that need a specific
	// value must set it before the first Observe call starts the writer).
	WriteBudget time.Duration

	busy atomic.Bool

	stats statCounters

	writerOnce sync.Once
	writeCh    chan writeJob
	// inflight tracks jobs handed to writeCh that the writer goroutine has
	// not finished processing yet, including the one it is actively
	// writing. Used only by WaitForIdle (tests / diagnostics) to detect a
	// fully-drained queue without a fixed sleep.
	inflight sync.WaitGroup
}

// writeJob is one persistence write handed from Observe's evaluating
// goroutine to the background writer goroutine.
type writeJob struct {
	in     Input
	result Result
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
	//
	// Because persistence now happens asynchronously on the background
	// writer (see Observer.writeCh), PersistErr on the Result Observe
	// returns to its caller is always nil: the write has not been
	// attempted yet at that point. It remains a field on Result because
	// the background writer constructs its own Result internally (for the
	// slog.Warn call in recordNow) using the same type; callers of Observe
	// itself should not read this field.
	PersistErr error
}

// Stats is a point-in-time snapshot of Observer's in-process counters (see
// Observer.Stats). Every field is a total since process start (or since the
// Observer was constructed) — not a rate — matching this package's existing
// "best-effort, logging/dashboard only" posture: these are read with plain
// atomic loads, never reset, and never wired into Prometheus or any other
// external metrics system (out of scope for D03 — see CHE-685 review B3,
// which asked for exactly this in-process counter shape and nothing more).
type Stats struct {
	// EligibleTotal counts every Observe call that reached the cap-1 gate
	// check, i.e. every call except ones short-circuited by a nil Observer
	// or nil Store (those never attempt to observe anything at all).
	EligibleTotal int64
	// ShedPoolBusyTotal counts observations shed because the cap-1 gate was
	// already held.
	ShedPoolBusyTotal int64
	// ShedBudgetExceededTotal counts observations shed because Evaluate did
	// not finish within Budget.
	ShedBudgetExceededTotal int64
	// ErrorTotal counts observations that reached ReasonError (nil
	// Provider, or a panic inside the evaluate goroutine).
	ErrorTotal int64
	// PersistedTotal counts background writes that successfully inserted a
	// governance_receipt row, regardless of that row's status/shed_reason.
	PersistedTotal int64
	// PersistFailedTotal counts background writes whose
	// Store.InsertGovernanceReceipt call itself returned an error
	// (including a WriteBudget timeout).
	PersistFailedTotal int64
	// WriteQueueFullTotal counts terminal outcomes dropped because the
	// background writer's bounded queue was full when Observe tried to
	// enqueue them (see ReasonWriteQueueFull). These never reach
	// Store.InsertGovernanceReceipt at all, so they are not part of
	// PersistFailedTotal.
	WriteQueueFullTotal int64
}

// counters holds the atomic.Int64 backing fields for Stats. Kept as a
// separate unexported struct (rather than atomic.Int64 fields directly on
// Stats) so Stats itself stays a plain value type safe to copy out of
// Observer.Stats for a caller to read without racing further increments.
type statCounters struct {
	eligibleTotal           atomic.Int64
	shedPoolBusyTotal       atomic.Int64
	shedBudgetExceededTotal atomic.Int64
	errorTotal              atomic.Int64
	persistedTotal          atomic.Int64
	persistFailedTotal      atomic.Int64
	writeQueueFullTotal     atomic.Int64
}

// Observe runs one bounded, best-effort observation and enqueues its
// outcome for background persistence. It NEVER returns an error to the
// caller: every exit path (pool busy, budget exceeded, provider error,
// decided) returns a Result for logging only. The evaluation itself never
// blocks past Budget, and — unlike an earlier version of this package —
// Observe never performs the persistence DB write itself: it hands the
// Result to a bounded background queue and returns immediately after, so
// Budget is the caller's entire latency exposure (see the package doc and
// CHE-685 review B2).
//
// ctx is accepted for interface symmetry with the rest of this codebase's
// context-threading convention, but Observe deliberately builds its own
// context.WithTimeout(context.Background(), ...) window for evaluation
// rather than deriving from it: ctx is normally an HTTP request context that
// CreateComment/UpdateComment already returned a response on (or is about
// to) by the time this best-effort hook runs, and a caller-cancelled
// request must not be able to cut short an evaluation the comment author
// will never see the result of anyway. The background write goes even
// further and never touches ctx at all — see startWriter's doc.
func (o *Observer) Observe(_ context.Context, in Input) Result {
	if o == nil || o.Store == nil {
		return Result{Status: "error", ShedReason: ReasonError}
	}
	o.startWriter()
	o.stats.eligibleTotal.Add(1)

	if !o.busy.CompareAndSwap(false, true) {
		o.stats.shedPoolBusyTotal.Add(1)
		result := Result{Status: "shed", ShedReason: ReasonPoolBusy}
		o.enqueueWrite(in, result)
		return result
	}

	ctx, cancel := context.WithTimeout(context.Background(), Budget)
	defer cancel()

	result, done := o.evaluate(ctx, in)
	// releaseBusy only fires once (see its own doc): either here, on the
	// normal completed-within-Budget path, or later from the goroutine
	// evaluate started, once a still-running provider call actually
	// returns. Never both.
	o.releaseBusyWhenDone(done)
	switch {
	case result.Status == "shed" && result.ShedReason == ReasonBudgetExceeded:
		o.stats.shedBudgetExceededTotal.Add(1)
	case result.Status == "error":
		o.stats.errorTotal.Add(1)
	}
	o.enqueueWrite(in, result)
	return result
}

// releaseBusyWhenDone frees the cap-1 gate exactly once. If done is already
// closed (the common case: evaluation finished inside Budget) the release
// happens synchronously on the caller's own goroutine with no extra
// scheduling delay. If done is still open — the provider ran past Budget
// and evaluate already returned a budget_exceeded shed to unblock Observe's
// caller — this spawns a short-lived goroutine that waits for the
// still-running provider call to actually finish before releasing busy.
//
// This is CHE-685 review N1: releasing busy immediately at budget_exceeded
// (the previous behavior, via a plain `defer o.busy.Store(false)`) let a
// second Observe be admitted while the first provider call was still
// physically in flight, so two calls to a slow provider could overlap. That
// never mattered while Provider is nil in production (a nil Provider always
// returns immediately, before Budget can even be reached — see evaluate),
// but it must hold before a live Jev provider is wired. The extra goroutine
// here is bounded by the same done channel evaluate already uses to detect
// provider completion, so it adds no new unbounded-wait risk: a provider
// that hangs forever already hangs its own evaluate goroutine forever today
// (a pre-existing, unrelated risk this fix does not change or enlarge).
func (o *Observer) releaseBusyWhenDone(done <-chan struct{}) {
	select {
	case <-done:
		o.busy.Store(false)
	default:
		go func() {
			<-done
			o.busy.Store(false)
		}()
	}
}

// Stats returns a snapshot of Observer's in-process counters (CHE-685
// review B3). A nil Observer returns the zero Stats rather than panicking,
// matching every other nil-safe method on this type.
func (o *Observer) Stats() Stats {
	if o == nil {
		return Stats{}
	}
	return Stats{
		EligibleTotal:           o.stats.eligibleTotal.Load(),
		ShedPoolBusyTotal:       o.stats.shedPoolBusyTotal.Load(),
		ShedBudgetExceededTotal: o.stats.shedBudgetExceededTotal.Load(),
		ErrorTotal:              o.stats.errorTotal.Load(),
		PersistedTotal:          o.stats.persistedTotal.Load(),
		PersistFailedTotal:      o.stats.persistFailedTotal.Load(),
		WriteQueueFullTotal:     o.stats.writeQueueFullTotal.Load(),
	}
}

// startWriter lazily starts the single background writer goroutine on first
// use. sync.Once makes this safe under the same concurrent-Observe-calls
// conditions the cap-1 gate already has to tolerate. There is deliberately
// no explicit Stop/Shutdown: this package has no caller-visible external
// resource to release (no connection, no file handle — Store's own
// lifecycle is owned by whoever constructs the Observer), so the writer
// goroutine simply runs for the lifetime of the process, exiting only if
// the process itself exits. Wiring this into the sweepCtx-bound worker
// convention other long-lived goroutines in this codebase use (see
// cmd/server/main.go's `go h.SeatCapacityWorker.Run(sweepCtx)` and similar)
// is a reasonable future improvement but was judged out of scope for a
// CHE-685 review-fix pass: it would require new Handler/router/main wiring
// this package does not otherwise touch, purely to add a shutdown signal to
// a goroutine that has nothing to flush or close on exit (queued writes
// that lose the race with process exit are simply lost, same as they would
// be if the process died mid-request before this change).
func (o *Observer) startWriter() {
	o.writerOnce.Do(func() {
		o.writeCh = make(chan writeJob, writeQueueCapacity)
		go o.runWriter()
	})
}

// enqueueWrite hands one outcome to the background writer without ever
// blocking the caller. A full queue means the writer is falling behind
// (e.g. sustained DB slowness) — in that case the outcome is dropped and
// counted via ReasonWriteQueueFull/Stats.WriteQueueFullTotal rather than
// letting the request goroutine block on a full channel send, which would
// silently reintroduce the exact synchronous-write latency this change
// removes.
func (o *Observer) enqueueWrite(in Input, result Result) {
	o.inflight.Add(1)
	select {
	case o.writeCh <- writeJob{in: in, result: result}:
	default:
		o.inflight.Done()
		o.stats.writeQueueFullTotal.Add(1)
		slog.Warn("governance receipt write queue full; dropping outcome",
			"issue_id", in.IssueID,
			"comment_id", in.CommentID,
			"status", result.Status,
		)
	}
}

// runWriter drains writeCh for the lifetime of the process (see startWriter
// for why there is no shutdown path). It is the single consumer, so writes
// are persisted one at a time in enqueue order — there is no concurrency to
// reason about inside recordNow itself.
func (o *Observer) runWriter() {
	for job := range o.writeCh {
		o.recordNow(job.in, job.result)
		o.inflight.Done()
	}
}

// WaitForIdle blocks until every write enqueued before this call was called
// has been handed to Store.InsertGovernanceReceipt (successfully or not).
// It exists for tests and diagnostics that need a deterministic point after
// which governance_receipt rows for already-completed Observe calls are
// guaranteed to be visible, instead of a fixed sleep racing the background
// writer. Production code must never call this: it is the one place in this
// package that reintroduces a wait for the background write, and it is
// intentionally not on the request path. A nil Observer or one whose writer
// never started (no Observe call yet) returns immediately.
func (o *Observer) WaitForIdle() {
	if o == nil {
		return
	}
	o.inflight.Wait()
}

// recordNow performs one persistence write with its own fresh
// WriteBudget-bounded context.Background() derivation. It runs exclusively
// on the background writer goroutine (see runWriter) — never on a request
// goroutine — so a slow or momentarily-contended database only ever delays
// the next queued write, never an HTTP caller.
func (o *Observer) recordNow(in Input, result Result) {
	budget := o.WriteBudget
	if budget <= 0 {
		budget = DefaultWriteBudget
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	o.record(ctx, in, result)
}

// evaluate runs governance.Evaluate and classifies its outcome into a
// Result, without touching storage. The returned channel closes once the
// spawned provider goroutine has fully returned — including when that
// happens strictly after evaluate itself already returned a
// budget_exceeded Result to its caller — so releaseBusyWhenDone can key the
// cap-1 gate's release off actual provider-call completion rather than off
// evaluate's own return (see CHE-685 review N1 and releaseBusyWhenDone's
// doc). A nil Provider returns an already-closed channel: there is no
// goroutine in flight to wait for.
func (o *Observer) evaluate(ctx context.Context, in Input) (Result, <-chan struct{}) {
	if o.Provider == nil {
		closed := make(chan struct{})
		close(closed)
		return Result{Status: "error", ShedReason: ReasonError}, closed
	}

	type outcome struct {
		decision governance.Decision
		err      error
	}
	done := make(chan outcome, 1)
	goroutineDone := make(chan struct{})
	go func() {
		defer close(goroutineDone)
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
		return Result{Status: "shed", ShedReason: ReasonBudgetExceeded}, goroutineDone
	case out := <-done:
		return decisionResult(out.decision, out.err), goroutineDone
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
// this is the "storage failure never fails the request" guarantee. It is
// enforced here at the one call site that ever writes a receipt row (now
// only ever invoked from the background writer goroutine — see recordNow),
// rather than trusted to every future caller of Observe to remember.
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
		o.stats.persistFailedTotal.Add(1)
		slog.Warn("governance receipt persist failed",
			"error", err,
			"issue_id", in.IssueID,
			"comment_id", in.CommentID,
			"status", result.Status,
		)
		return
	}
	o.stats.persistedTotal.Add(1)
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
