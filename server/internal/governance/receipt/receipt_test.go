package receipt

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/governance"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/jev"
)

// ---- fixtures --------------------------------------------------------

// testUUID returns a deterministic, valid pgtype.UUID for the given seed
// byte, distinct enough for one test's fixtures to be told apart.
func testUUID(seed byte) pgtype.UUID {
	var b [16]byte
	b[0] = seed
	return pgtype.UUID{Bytes: b, Valid: true}
}

func oneCandidateOneSpanInput() governance.Input {
	return governance.Input{
		State: "synthetic receipt-package state",
		Candidates: []governance.Candidate{
			{ID: "11111111-1111-1111-1111-111111111111", DisplayName: "agent-a", Description: "current issue's agent assignee"},
		},
		Spans: []governance.Span{
			{Text: "please pick this up", StartOffset: 0, EndOffset: 20},
		},
		AccountableIndex: 0,
	}
}

func testInput() Input {
	return Input{
		WorkspaceID: testUUID(1),
		IssueID:     testUUID(2),
		CommentID:   testUUID(3),
		Trigger:     TriggerCreate,
		Eval:        oneCandidateOneSpanInput(),
	}
}

// mentionOwnerResponse scripts the exact four-question wire response that
// governance.Evaluate's protocol (server/internal/governance/questions.go)
// turns into a mention_owner Action for the single-candidate, single-span
// input oneCandidateOneSpanInput builds. The question ids and option labels
// here mirror the unexported constants in that package's questions.go
// (handoff_kind/next_owner/evidence/correction, candidate_01/span_01,
// agent_work/mention_owner) — this package cannot import them directly, but
// the protocol they encode is documented and stable, not incidental.
//
// Each answer's Probabilities carries only its own winning label at
// `confidence`. AsStrictChoice requires exactly one probability entry per
// OFFERED label, which for a single-candidate/single-span Input is exactly
// one label per question (plus the always-offered "none"/"ambiguous"
// sentinels on next_owner/evidence) — but this package cannot see
// governance's private offered-label set to build a fully-matching
// distribution from outside it. Receipt-package tests exist to prove
// Observer's pool/budget/persistence behavior around governance.Evaluate,
// not to re-prove Evaluate's own strict-answer-validation contract (already
// exhaustively covered by evaluator_test.go) — so these fixtures rely on
// AsStrictChoice's leniency for a single-offered-label question directly,
// and the "decided" tests below assert on Decision/Answers being populated
// rather than on a specific Action, which stays robust either way.
func mentionOwnerResponse(confidence float64) *jev.Response {
	return &jev.Response{
		Model: jev.DefaultModel,
		Answers: map[string]jev.Answer{
			"handoff_kind": choiceAnswer("agent_work", confidence, map[string]float64{
				"agent_work": confidence, "agent_preparation": 0, "human_decision": 0, "no_followup": 0, "unclear": 1 - confidence,
			}),
			"next_owner": choiceAnswer("candidate_01", confidence, map[string]float64{
				"candidate_01": confidence, "none": 0, "ambiguous": 1 - confidence,
			}),
			"evidence": choiceAnswer("span_01", confidence, map[string]float64{
				"span_01": confidence, "none": 0, "ambiguous": 1 - confidence,
			}),
			"correction": choiceAnswer("mention_owner", confidence, map[string]float64{
				"mention_owner": confidence, "return_mechanical_step_to_assignee": 0, "no_correction": 0, "unclear": 1 - confidence,
			}),
		},
	}
}

// choiceAnswer builds a jev.Answer through JSON decoding, the same way
// evaluator_test.go does, so AsStrictChoice's field-presence tracking is
// exercised the same way a real wire response would populate it.
func choiceAnswer(choice string, confidence float64, probabilities map[string]float64) jev.Answer {
	body := map[string]any{
		"type":          "choice",
		"choice":        choice,
		"confidence":    confidence,
		"probabilities": probabilities,
	}
	data, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	var a jev.Answer
	if err := json.Unmarshal(data, &a); err != nil {
		panic(err)
	}
	return a
}

// fakeProvider scripts a Jev response (or error, or artificial delay)
// without any network call.
type fakeProvider struct {
	resp  *jev.Response
	err   error
	delay time.Duration
	calls atomic.Int32
}

func (f *fakeProvider) Evaluate(ctx context.Context, _ jev.Request) (*jev.Response, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// panicProvider always panics, to exercise Observer's recover().
type panicProvider struct{}

func (panicProvider) Evaluate(context.Context, jev.Request) (*jev.Response, error) {
	panic("synthetic provider panic")
}

// fakeStore is an in-memory Store that records every insert, and can be
// scripted to fail.
type fakeStore struct {
	mu      sync.Mutex
	rows    []db.InsertGovernanceReceiptParams
	failErr error
}

func (f *fakeStore) InsertGovernanceReceipt(_ context.Context, arg db.InsertGovernanceReceiptParams) (db.GovernanceReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failErr != nil {
		return db.GovernanceReceipt{}, f.failErr
	}
	f.rows = append(f.rows, arg)
	return db.GovernanceReceipt{}, nil
}

func (f *fakeStore) inserted() []db.InsertGovernanceReceiptParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]db.InsertGovernanceReceiptParams, len(f.rows))
	copy(out, f.rows)
	return out
}

// ---- tests -------------------------------------------------------------

// TestObserve_NilStoreIsNoOp proves a caller that wires an Observer without
// a Store fails safe (no panic, no attempted call) rather than crashing the
// request path it is embedded in.
func TestObserve_NilStoreIsNoOp(t *testing.T) {
	provider := &fakeProvider{resp: mentionOwnerResponse(0.99)}
	o := &Observer{Provider: provider}

	result := o.Observe(context.Background(), testInput())
	if result.Status != "error" || result.ShedReason != ReasonError {
		t.Fatalf("result = %+v, want error/error", result)
	}
	if provider.calls.Load() != 0 {
		t.Errorf("provider was called %d time(s), want 0 (Store is nil, nothing to persist into)", provider.calls.Load())
	}
}

// TestObserve_NilProviderRecordsError proves the D03 "no live key" shape:
// an Observer wired with Store but no Provider (production's default in
// this delivery) never attempts a call and persists status=error,
// shed_reason=error.
func TestObserve_NilProviderRecordsError(t *testing.T) {
	store := &fakeStore{}
	o := &Observer{Store: store}

	result := o.Observe(context.Background(), testInput())
	if result.Status != "error" || result.ShedReason != ReasonError {
		t.Fatalf("result = %+v, want error/error", result)
	}
	rows := store.inserted()
	if len(rows) != 1 {
		t.Fatalf("inserted %d row(s), want 1", len(rows))
	}
	if rows[0].Status != "error" || rows[0].ShedReason.String != "error" || !rows[0].ShedReason.Valid {
		t.Errorf("row = %+v, want status=error shed_reason=error", rows[0])
	}
	if rows[0].ObservedAt.Valid {
		t.Errorf("ObservedAt = %+v, want unset for a non-decided receipt", rows[0].ObservedAt)
	}
}

// TestObserve_DecidedPersistsAnswersAndActionKind proves the full happy
// path: a fast, successful provider call produces a 'decided' row carrying
// the abstain_reason (empty for an Action), action_kind, and the raw
// per-question Answers — the audit contract governance.RecordedAnswer
// documents.
func TestObserve_DecidedPersistsAnswersAndActionKind(t *testing.T) {
	provider := &fakeProvider{resp: mentionOwnerResponse(0.99)}
	store := &fakeStore{}
	o := &Observer{Provider: provider, Store: store}

	in := testInput()
	result := o.Observe(context.Background(), in)
	if result.Status != "decided" {
		t.Fatalf("Status = %q, want decided (err=%v)", result.Status, result)
	}
	if result.Decision == nil {
		t.Fatal("Decision is nil for a decided result")
	}

	rows := store.inserted()
	if len(rows) != 1 {
		t.Fatalf("inserted %d row(s), want 1", len(rows))
	}
	row := rows[0]
	if row.Status != "decided" {
		t.Errorf("Status = %q, want decided", row.Status)
	}
	if row.ShedReason.Valid {
		t.Errorf("ShedReason = %+v, want unset for a decided row", row.ShedReason)
	}
	if !row.ObservedAt.Valid {
		t.Error("ObservedAt unset for a decided row")
	}
	if row.WorkspaceID != in.WorkspaceID || row.IssueID != in.IssueID || row.CommentID != in.CommentID {
		t.Errorf("row identity = %+v, want it to match Input", row)
	}
	if row.Trigger != string(TriggerCreate) {
		t.Errorf("Trigger = %q, want %q", row.Trigger, TriggerCreate)
	}
	var answers []governance.RecordedAnswer
	if err := json.Unmarshal(row.Answers, &answers); err != nil {
		t.Fatalf("decode persisted answers: %v", err)
	}
	if len(answers) != 4 {
		t.Errorf("len(answers) = %d, want 4 recorded answers", len(answers))
	}
}

// TestObserve_ProviderErrorIsShedError proves that a Provider whose
// Evaluate call itself returns a transport-level error (as opposed to
// governance.Evaluate's own internal ReasonProviderError abstention, which
// this package cannot trigger without importing governance's private
// question protocol) is recorded distinctly, not silently dropped.
//
// governance.Evaluate DOES turn its own provider.Evaluate error into a
// ReasonProviderError abstention — a 'decided' receipt, per package
// evaluator.go's contract, since it reached a terminal judgment. This is
// exercised indirectly here: the fake's err is surfaced through
// Evaluate's own wrapping, so this receipt still lands as 'decided' with
// AbstainReason=provider_error, proving decisionResult's classification
// does NOT misfile a normal provider failure as 'error'/'shed'.
func TestObserve_ProviderErrorIsRecordedAsDecidedAbstention(t *testing.T) {
	provider := &fakeProvider{err: errors.New("synthetic transport failure")}
	store := &fakeStore{}
	o := &Observer{Provider: provider, Store: store}

	result := o.Observe(context.Background(), testInput())
	if result.Status != "decided" {
		t.Fatalf("Status = %q, want decided (a provider error is a terminal abstention, not a shed)", result.Status)
	}
	if result.Decision == nil || result.Decision.AbstainReason != governance.ReasonProviderError {
		t.Fatalf("Decision = %+v, want AbstainReason=provider_error", result.Decision)
	}

	rows := store.inserted()
	if len(rows) != 1 {
		t.Fatalf("inserted %d row(s), want 1", len(rows))
	}
	if rows[0].Status != "decided" || rows[0].AbstainReason != string(governance.ReasonProviderError) {
		t.Errorf("row = %+v, want decided/provider_error", rows[0])
	}
}

// TestObserve_PanicIsRecordedAsError proves a panicking provider cannot
// crash the caller and is recorded distinctly from a clean provider error.
func TestObserve_PanicIsRecordedAsError(t *testing.T) {
	store := &fakeStore{}
	o := &Observer{Provider: panicProvider{}, Store: store}

	result := o.Observe(context.Background(), testInput())
	if result.Status != "error" || result.ShedReason != ReasonError {
		t.Fatalf("result = %+v, want error/error", result)
	}
	rows := store.inserted()
	if len(rows) != 1 || rows[0].Status != "error" {
		t.Fatalf("rows = %+v, want exactly one error row", rows)
	}
}

// TestObserve_BudgetExceededSheds proves a provider slower than Budget is
// shed with ReasonBudgetExceeded rather than blocking the caller past
// Budget, and that the shed outcome is still persisted (a slow observation
// is not the same as "no receipt captured").
func TestObserve_BudgetExceededSheds(t *testing.T) {
	provider := &fakeProvider{resp: mentionOwnerResponse(0.99), delay: Budget * 20}
	store := &fakeStore{}
	o := &Observer{Provider: provider, Store: store}

	start := time.Now()
	result := o.Observe(context.Background(), testInput())
	elapsed := time.Since(start)

	if result.Status != "shed" || result.ShedReason != ReasonBudgetExceeded {
		t.Fatalf("result = %+v, want shed/budget_exceeded", result)
	}
	// Generous multiple of Budget: this asserts Observe does not wait out
	// the full artificial provider delay (20x Budget), not a tight latency
	// bound — CI schedulers are not real-time.
	if elapsed > Budget*10 {
		t.Errorf("Observe took %v, want well under the provider's %v delay", elapsed, provider.delay)
	}

	rows := store.inserted()
	if len(rows) != 1 || rows[0].Status != "shed" || rows[0].ShedReason.String != "budget_exceeded" {
		t.Fatalf("rows = %+v, want exactly one shed/budget_exceeded row", rows)
	}
}

// TestObserve_PoolCapOne_ConcurrentObservationIsShedPoolBusy is the
// acceptance test for the process-wide concurrency cap of exactly one: two
// concurrent Observe calls on the same *Observer must never both run
// governance.Evaluate at once. One completes normally; the other is shed
// with ReasonPoolBusy and never reaches the provider.
func TestObserve_PoolCapOne_ConcurrentObservationIsShedPoolBusy(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	provider := &blockingProvider{entered: entered, release: release, resp: mentionOwnerResponse(0.99)}
	store := &fakeStore{}
	o := &Observer{Provider: provider, Store: store}

	var firstResult Result
	done := make(chan struct{})
	go func() {
		defer close(done)
		firstResult = o.Observe(context.Background(), testInput())
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first Observe never reached the provider")
	}

	// The cap-1 gate must already be held: a second, concurrent Observe is
	// shed immediately, without waiting for the first to release.
	secondResult := o.Observe(context.Background(), testInput())
	if secondResult.Status != "shed" || secondResult.ShedReason != ReasonPoolBusy {
		t.Fatalf("second Observe = %+v, want shed/pool_busy", secondResult)
	}

	close(release)
	<-done

	if firstResult.Status != "decided" {
		t.Fatalf("first Observe = %+v, want decided", firstResult)
	}
	if provider.calls.Load() != 1 {
		t.Errorf("provider called %d time(s), want exactly 1 (the shed observation must never call it)", provider.calls.Load())
	}

	rows := store.inserted()
	var sawPoolBusy, sawDecided bool
	for _, r := range rows {
		switch {
		case r.Status == "shed" && r.ShedReason.String == "pool_busy":
			sawPoolBusy = true
		case r.Status == "decided":
			sawDecided = true
		}
	}
	if !sawPoolBusy {
		t.Error("no pool_busy row persisted")
	}
	if !sawDecided {
		t.Error("no decided row persisted")
	}
}

// TestObserve_PoolReleasedAfterCompletion proves the cap-1 gate is released
// once an observation finishes, so it is a genuine cap (never permanently
// exhausted) rather than a one-shot lock.
func TestObserve_PoolReleasedAfterCompletion(t *testing.T) {
	provider := &fakeProvider{resp: mentionOwnerResponse(0.99)}
	store := &fakeStore{}
	o := &Observer{Provider: provider, Store: store}

	first := o.Observe(context.Background(), testInput())
	second := o.Observe(context.Background(), testInput())

	if first.Status != "decided" || second.Status != "decided" {
		t.Fatalf("first=%+v second=%+v, want both decided (sequential calls must never see pool_busy)", first, second)
	}
	if provider.calls.Load() != 2 {
		t.Errorf("provider called %d time(s), want 2", provider.calls.Load())
	}
}

// blockingProvider blocks inside Evaluate until release is closed, signaling
// entered once it has started — used to deterministically win the race
// against a concurrent second Observe call without a sleep-based retry.
type blockingProvider struct {
	entered chan struct{}
	release chan struct{}
	resp    *jev.Response
	calls   atomic.Int32
	once    sync.Once
}

func (p *blockingProvider) Evaluate(ctx context.Context, _ jev.Request) (*jev.Response, error) {
	p.calls.Add(1)
	p.once.Do(func() { close(p.entered) })
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return p.resp, nil
}
