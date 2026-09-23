package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/governance"
	"github.com/multica-ai/multica/server/internal/governance/receipt"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
	"github.com/multica-ai/multica/server/pkg/jev"
)

// ---- shared test fixtures / fakes --------------------------------------

// governanceFakeProvider scripts a fixed Jev outcome for every call. When
// resp is nil and err is nil it still returns a minimal empty response so
// governance.Evaluate does not hit a nil-response abstention by accident in
// tests that don't care about the exact Decision.
type governanceFakeProvider struct {
	err error
}

func (p *governanceFakeProvider) Evaluate(_ context.Context, req jev.Request) (*jev.Response, error) {
	if p.err != nil {
		return nil, p.err
	}
	// Build a response that abstains cleanly (low_confidence) regardless of
	// exactly what was asked: these regression tests only need governance
	// to REACH a decision, never to produce any specific Action, since no
	// action is ever supposed to change comment/routing behavior anyway.
	answers := make(map[string]jev.Answer, len(req.Questions))
	for id, q := range req.Questions {
		options, _ := q.Criteria.(map[string]*string)
		var winner string
		for label := range options {
			winner = label
			break
		}
		probs := map[string]float64{winner: 0.05}
		remaining := len(options) - 1
		if remaining > 0 {
			each := 0.95 / float64(remaining)
			for label := range options {
				if label != winner {
					probs[label] = each
				}
			}
		} else {
			probs[winner] = 1
		}
		answers[id] = jevChoiceAnswer(winner, 0.05, probs)
	}
	return &jev.Response{Model: jev.DefaultModel, Answers: answers}, nil
}

// jevChoiceAnswer builds a jev.Answer through JSON decoding so the
// unexported field-presence tracking AsStrictChoice depends on is populated
// the same way a real wire response would.
func jevChoiceAnswer(choice string, confidence float64, probabilities map[string]float64) jev.Answer {
	body := map[string]any{
		"type": "choice", "choice": choice, "confidence": confidence, "probabilities": probabilities,
	}
	data, _ := json.Marshal(body)
	var a jev.Answer
	_ = json.Unmarshal(data, &a)
	return a
}

// failingGovernanceStore always fails the persistence write, to prove a
// storage failure never surfaces to the HTTP caller.
type failingGovernanceStore struct{}

func (failingGovernanceStore) InsertGovernanceReceipt(context.Context, db.InsertGovernanceReceiptParams) (db.GovernanceReceipt, error) {
	return db.GovernanceReceipt{}, errors.New("synthetic storage failure")
}

// withGovernanceFlag flips featureflags.JevReceipts for the duration of one
// test and restores the handler's previous FeatureFlags afterward.
func withGovernanceFlag(t *testing.T, enabled bool) {
	t.Helper()
	previous := testHandler.FeatureFlags
	sp := featureflag.NewStaticProvider()
	sp.LoadRules(map[string]featureflag.Rule{featureflags.JevReceipts: {Default: enabled}})
	testHandler.FeatureFlags = featureflag.NewService(sp)
	t.Cleanup(func() { testHandler.FeatureFlags = previous })
}

// withGovernanceObserver swaps in a fresh *receipt.Observer for the
// duration of one test (each test gets its own cap-1 gate) and restores the
// handler's previous one afterward.
func withGovernanceObserver(t *testing.T, provider governance.Provider, store receipt.Store) {
	t.Helper()
	withGovernanceObserverWriteBudget(t, provider, store, 0)
}

// withGovernanceObserverWriteBudget is withGovernanceObserver plus an
// explicit Observer.WriteBudget for the duration of one test. writeBudget<=0
// falls back to receipt.DefaultWriteBudget, same as a production Observer.
// CHE-685 review B1 asked for exactly this: an injectable write deadline so
// a test can prove persistence succeeds under a generous budget or sheds
// under a near-zero one, deterministically, instead of depending on how
// fast the real test database happens to respond on a given CI run.
func withGovernanceObserverWriteBudget(t *testing.T, provider governance.Provider, store receipt.Store, writeBudget time.Duration) {
	t.Helper()
	previous := testHandler.GovernanceReceipts
	testHandler.GovernanceReceipts = &receipt.Observer{Provider: provider, Store: store, WriteBudget: writeBudget}
	t.Cleanup(func() { testHandler.GovernanceReceipts = previous })
}

// slowGovernanceStore wraps a real Store and sleeps before delegating, so a
// test can force the background writer's WriteBudget to be exceeded without
// depending on real database slowness.
type slowGovernanceStore struct {
	inner receipt.Store
	delay time.Duration
}

func (s slowGovernanceStore) InsertGovernanceReceipt(ctx context.Context, arg db.InsertGovernanceReceiptParams) (db.GovernanceReceipt, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return db.GovernanceReceipt{}, ctx.Err()
	}
	return s.inner.InsertGovernanceReceipt(ctx, arg)
}

func governanceReceiptCountForComment(t *testing.T, commentID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM governance_receipt WHERE comment_id = $1`, commentID,
	).Scan(&n); err != nil {
		t.Fatalf("count governance_receipt rows: %v", err)
	}
	return n
}

func governanceReceiptStatusesForComment(t *testing.T, commentID string) []string {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT status FROM governance_receipt WHERE comment_id = $1 ORDER BY created_at`, commentID)
	if err != nil {
		t.Fatalf("query governance_receipt statuses: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan status: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// normalizedCommentResponse zeroes the fields that legitimately differ
// between two independently created comments (identity, timestamps,
// revision counters) so two responses can be compared for the rest of
// their shape being byte-identical.
func normalizedCommentResponse(t *testing.T, body []byte) CommentResponse {
	t.Helper()
	var resp CommentResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode CommentResponse: %v\nbody: %s", err, body)
	}
	resp.ID = ""
	resp.IssueID = ""
	resp.CreatedAt = ""
	resp.UpdatedAt = ""
	resp.Revision = 0
	resp.IssueRevision = 0
	resp.SourceTaskID = nil
	return resp
}

// assertNormalizedResponsesEqual compares two normalized responses field by
// field (CommentResponse embeds slices, so plain == does not compile).
func assertNormalizedResponsesEqual(t *testing.T, got, want CommentResponse) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("response shape diverged from baseline:\n got  = %s\n want = %s", gotJSON, wantJSON)
	}
}

// ---- CreateComment regression matrix ------------------------------------
//
// CHE-685's core regression contract: adding the governance receipt hook
// must change NOTHING observable about CreateComment's HTTP response,
// across every combination of {flag on/off} x {governance outcome}. Each
// case below posts an otherwise-identical comment and asserts the response
// shape (modulo per-comment identity/timestamp fields) matches a governance-
// hook-free baseline captured with the flag off.

func createCommentForGovernanceTest(t *testing.T, issueID, content string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	w := httptest.NewRecorder()
	r := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content}), "id", issueID)
	testHandler.CreateComment(w, r)
	var resp CommentResponse
	if w.Code == http.StatusCreated {
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
	}
	return w, resp.ID
}

func TestCreateComment_GovernanceFlagOff_ResponseUnaffectedAndNoReceiptWritten(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	issueID := createCommentTriggerPreviewIssue(t, "governance flag off create", "member", testUserID)

	withGovernanceFlag(t, false)
	withGovernanceObserver(t, &governanceFakeProvider{}, testHandler.Queries)

	w, commentID := createCommentForGovernanceTest(t, issueID, "plain comment, governance flag off")
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateComment: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if commentID == "" {
		t.Fatal("comment was not saved")
	}
	if got := governanceReceiptCountForComment(t, commentID); got != 0 {
		t.Errorf("governance_receipt rows = %d, want 0 (flag is off, hook must be a true no-op)", got)
	}
}

func TestCreateComment_GovernanceFlagOn_ProviderSucceeds_ResponseUnaffected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	baselineIssueID := createCommentTriggerPreviewIssue(t, "governance baseline create", "member", testUserID)
	withGovernanceFlag(t, false)
	baselineW, _ := createCommentForGovernanceTest(t, baselineIssueID, "identical content across all four cases")
	if baselineW.Code != http.StatusCreated {
		t.Fatalf("baseline CreateComment: expected 201, got %d: %s", baselineW.Code, baselineW.Body.String())
	}
	baseline := normalizedCommentResponse(t, baselineW.Body.Bytes())

	issueID := createCommentTriggerPreviewIssue(t, "governance flag on success create", "member", testUserID)
	withGovernanceFlag(t, true)
	withGovernanceObserver(t, &governanceFakeProvider{}, testHandler.Queries)

	w, commentID := createCommentForGovernanceTest(t, issueID, "identical content across all four cases")
	if w.Code != baselineW.Code {
		t.Fatalf("status code = %d, want %d (baseline)", w.Code, baselineW.Code)
	}
	got := normalizedCommentResponse(t, w.Body.Bytes())
	assertNormalizedResponsesEqual(t, got, baseline)
	// Persistence happens on Observer's background writer, off the request
	// path (CHE-685 review B2) — WaitForIdle is the deterministic point
	// after which the queued write is guaranteed to have reached Store,
	// instead of a sleep racing that goroutine.
	testHandler.GovernanceReceipts.WaitForIdle()
	if got := governanceReceiptCountForComment(t, commentID); got != 1 {
		t.Fatalf("governance_receipt rows = %d, want 1 (flag on, fake provider succeeds)", got)
	}
	if statuses := governanceReceiptStatusesForComment(t, commentID); len(statuses) != 1 || statuses[0] != "decided" {
		t.Errorf("statuses = %v, want [decided]", statuses)
	}
}

func TestCreateComment_GovernanceFlagOn_ProviderErrors_ResponseUnaffected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	baselineIssueID := createCommentTriggerPreviewIssue(t, "governance baseline create err", "member", testUserID)
	withGovernanceFlag(t, false)
	baselineW, _ := createCommentForGovernanceTest(t, baselineIssueID, "identical content across all four cases")
	if baselineW.Code != http.StatusCreated {
		t.Fatalf("baseline CreateComment: expected 201, got %d: %s", baselineW.Code, baselineW.Body.String())
	}
	baseline := normalizedCommentResponse(t, baselineW.Body.Bytes())

	issueID := createCommentTriggerPreviewIssue(t, "governance flag on error create", "member", testUserID)
	withGovernanceFlag(t, true)
	withGovernanceObserver(t, &governanceFakeProvider{err: errors.New("synthetic provider failure")}, testHandler.Queries)

	w, commentID := createCommentForGovernanceTest(t, issueID, "identical content across all four cases")
	if w.Code != baselineW.Code {
		t.Fatalf("status code = %d, want %d (baseline)", w.Code, baselineW.Code)
	}
	got := normalizedCommentResponse(t, w.Body.Bytes())
	assertNormalizedResponsesEqual(t, got, baseline)
	// A provider error is still a 'decided' receipt (governance.Evaluate's
	// own contract turns it into a ReasonProviderError abstention) — see
	// server/internal/governance/receipt's decisionResult.
	testHandler.GovernanceReceipts.WaitForIdle()
	if got := governanceReceiptCountForComment(t, commentID); got != 1 {
		t.Fatalf("governance_receipt rows = %d, want 1 (flag on, fake provider errors)", got)
	}
}

func TestCreateComment_GovernanceFlagOn_StorageWriteFails_ResponseUnaffected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	baselineIssueID := createCommentTriggerPreviewIssue(t, "governance baseline create storefail", "member", testUserID)
	withGovernanceFlag(t, false)
	baselineW, _ := createCommentForGovernanceTest(t, baselineIssueID, "identical content across all four cases")
	if baselineW.Code != http.StatusCreated {
		t.Fatalf("baseline CreateComment: expected 201, got %d: %s", baselineW.Code, baselineW.Body.String())
	}
	baseline := normalizedCommentResponse(t, baselineW.Body.Bytes())

	issueID := createCommentTriggerPreviewIssue(t, "governance flag on storefail create", "member", testUserID)
	withGovernanceFlag(t, true)
	withGovernanceObserver(t, &governanceFakeProvider{}, failingGovernanceStore{})

	w, _ := createCommentForGovernanceTest(t, issueID, "identical content across all four cases")
	if w.Code != baselineW.Code {
		t.Fatalf("status code = %d, want %d (baseline) — a receipt-storage failure must never change the comment response", w.Code, baselineW.Code)
	}
	got := normalizedCommentResponse(t, w.Body.Bytes())
	assertNormalizedResponsesEqual(t, got, baseline)
}

// ---- UpdateComment regression matrix -------------------------------------

func TestUpdateComment_GovernanceFlagOff_ResponseUnaffectedAndNoReceiptWritten(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	issueID := createCommentTriggerPreviewIssue(t, "governance flag off edit", "member", testUserID)
	commentID := insertMemberRootCommentForTriggerPreviewTest(t, issueID, "original content")

	withGovernanceFlag(t, false)
	withGovernanceObserver(t, &governanceFakeProvider{}, testHandler.Queries)

	w := httptest.NewRecorder()
	r := withURLParam(newRequest(http.MethodPut, "/api/comments/"+commentID, map[string]any{"content": "edited content, flag off"}), "commentId", commentID)
	testHandler.UpdateComment(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateComment: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := governanceReceiptCountForComment(t, commentID); got != 0 {
		t.Errorf("governance_receipt rows = %d, want 0 (flag is off)", got)
	}
}

func updateCommentForGovernanceTest(t *testing.T, issueID, content string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	commentID := insertMemberRootCommentForTriggerPreviewTest(t, issueID, "original content")
	w := httptest.NewRecorder()
	r := withURLParam(newRequest(http.MethodPut, "/api/comments/"+commentID, map[string]any{"content": content}), "commentId", commentID)
	testHandler.UpdateComment(w, r)
	return w, commentID
}

func TestUpdateComment_GovernanceFlagOn_ProviderSucceeds_ResponseUnaffected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	baselineIssueID := createCommentTriggerPreviewIssue(t, "governance baseline edit", "member", testUserID)
	withGovernanceFlag(t, false)
	baselineW, _ := updateCommentForGovernanceTest(t, baselineIssueID, "identical edited content across all four cases")
	if baselineW.Code != http.StatusOK {
		t.Fatalf("baseline UpdateComment: expected 200, got %d: %s", baselineW.Code, baselineW.Body.String())
	}
	baseline := normalizedCommentResponse(t, baselineW.Body.Bytes())

	issueID := createCommentTriggerPreviewIssue(t, "governance flag on success edit", "member", testUserID)
	withGovernanceFlag(t, true)
	withGovernanceObserver(t, &governanceFakeProvider{}, testHandler.Queries)

	w, commentID := updateCommentForGovernanceTest(t, issueID, "identical edited content across all four cases")
	if w.Code != baselineW.Code {
		t.Fatalf("status code = %d, want %d (baseline)", w.Code, baselineW.Code)
	}
	got := normalizedCommentResponse(t, w.Body.Bytes())
	assertNormalizedResponsesEqual(t, got, baseline)
	// Persistence happens on Observer's background writer, off the request
	// path (CHE-685 review B2) — WaitForIdle is the deterministic point
	// after which the queued write is guaranteed to have reached Store,
	// instead of a sleep racing that goroutine.
	testHandler.GovernanceReceipts.WaitForIdle()
	if got := governanceReceiptCountForComment(t, commentID); got != 1 {
		t.Fatalf("governance_receipt rows = %d, want 1 (flag on, fake provider succeeds)", got)
	}
}

func TestUpdateComment_GovernanceFlagOn_ProviderErrors_ResponseUnaffected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	baselineIssueID := createCommentTriggerPreviewIssue(t, "governance baseline edit err", "member", testUserID)
	withGovernanceFlag(t, false)
	baselineW, _ := updateCommentForGovernanceTest(t, baselineIssueID, "identical edited content across all four cases")
	if baselineW.Code != http.StatusOK {
		t.Fatalf("baseline UpdateComment: expected 200, got %d: %s", baselineW.Code, baselineW.Body.String())
	}
	baseline := normalizedCommentResponse(t, baselineW.Body.Bytes())

	issueID := createCommentTriggerPreviewIssue(t, "governance flag on error edit", "member", testUserID)
	withGovernanceFlag(t, true)
	withGovernanceObserver(t, &governanceFakeProvider{err: errors.New("synthetic provider failure")}, testHandler.Queries)

	w, commentID := updateCommentForGovernanceTest(t, issueID, "identical edited content across all four cases")
	if w.Code != baselineW.Code {
		t.Fatalf("status code = %d, want %d (baseline)", w.Code, baselineW.Code)
	}
	got := normalizedCommentResponse(t, w.Body.Bytes())
	assertNormalizedResponsesEqual(t, got, baseline)
	testHandler.GovernanceReceipts.WaitForIdle()
	if got := governanceReceiptCountForComment(t, commentID); got != 1 {
		t.Fatalf("governance_receipt rows = %d, want 1 (flag on, fake provider errors)", got)
	}
}

func TestUpdateComment_GovernanceFlagOn_StorageWriteFails_ResponseUnaffected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	baselineIssueID := createCommentTriggerPreviewIssue(t, "governance baseline edit storefail", "member", testUserID)
	withGovernanceFlag(t, false)
	baselineW, _ := updateCommentForGovernanceTest(t, baselineIssueID, "identical edited content across all four cases")
	if baselineW.Code != http.StatusOK {
		t.Fatalf("baseline UpdateComment: expected 200, got %d: %s", baselineW.Code, baselineW.Body.String())
	}
	baseline := normalizedCommentResponse(t, baselineW.Body.Bytes())

	issueID := createCommentTriggerPreviewIssue(t, "governance flag on storefail edit", "member", testUserID)
	withGovernanceFlag(t, true)
	withGovernanceObserver(t, &governanceFakeProvider{}, failingGovernanceStore{})

	w, _ := updateCommentForGovernanceTest(t, issueID, "identical edited content across all four cases")
	if w.Code != baselineW.Code {
		t.Fatalf("status code = %d, want %d (baseline) — a receipt-storage failure must never change the comment response", w.Code, baselineW.Code)
	}
	got := normalizedCommentResponse(t, w.Body.Bytes())
	assertNormalizedResponsesEqual(t, got, baseline)
}

// ---- CHE-685 review B1/B2: injectable write budget, request latency -------
//
// The receipt hook's persistence write used to run synchronously on the
// request goroutine with a fixed 500ms deadline (writeBudget). That made
// this suite flaky under ordinary DB contention (CHE-685 review B1) and let
// every comment pay up to ~550ms of request latency for a receipt write the
// caller never sees (review B2). Persistence now runs on Observer's
// background writer, off the request path entirely, with an injectable
// WriteBudget — the tests below replace the old ambient-speed assertions
// with deterministic ones: a generous budget proves the row lands, a
// near-zero budget proves the write times out and is counted as failed.

// TestCreateComment_GovernanceOn_GenerousWriteBudgetPersistsDeterministically
// is CHE-685 review B1's DB-backed case: a real *db.Queries write against
// the real test database, under a deliberately generous WriteBudget, proves
// persistence succeeds without depending on how fast the test database
// happens to respond on a given CI run.
func TestCreateComment_GovernanceOn_GenerousWriteBudgetPersistsDeterministically(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	issueID := createCommentTriggerPreviewIssue(t, "governance generous write budget", "member", testUserID)

	withGovernanceFlag(t, true)
	withGovernanceObserverWriteBudget(t, &governanceFakeProvider{}, testHandler.Queries, 10*time.Second)

	w, commentID := createCommentForGovernanceTest(t, issueID, "generous write budget case")
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateComment: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	testHandler.GovernanceReceipts.WaitForIdle()
	if got := governanceReceiptCountForComment(t, commentID); got != 1 {
		t.Fatalf("governance_receipt rows = %d, want 1 (generous WriteBudget must persist deterministically)", got)
	}
	stats := testHandler.GovernanceReceipts.Stats()
	if stats.PersistedTotal < 1 {
		t.Errorf("Stats().PersistedTotal = %d, want >= 1", stats.PersistedTotal)
	}
}

// TestCreateComment_GovernanceOn_TightWriteBudgetShedsDeterministically is
// CHE-685 review B1's other half: a WriteBudget shorter than a scripted slow
// Store proves the write-timeout path deterministically, in place of the old
// test that only failed depending on ambient DB contention. The comment
// response itself must stay unaffected either way — persistence timing on
// the background writer can never reach the request path (review B2).
func TestCreateComment_GovernanceOn_TightWriteBudgetShedsDeterministically(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	baselineIssueID := createCommentTriggerPreviewIssue(t, "governance tight write budget baseline", "member", testUserID)
	withGovernanceFlag(t, false)
	baselineW, _ := createCommentForGovernanceTest(t, baselineIssueID, "identical content across all four cases")
	if baselineW.Code != http.StatusCreated {
		t.Fatalf("baseline CreateComment: expected 201, got %d: %s", baselineW.Code, baselineW.Body.String())
	}
	baseline := normalizedCommentResponse(t, baselineW.Body.Bytes())

	issueID := createCommentTriggerPreviewIssue(t, "governance tight write budget", "member", testUserID)
	withGovernanceFlag(t, true)
	slowStore := slowGovernanceStore{inner: testHandler.Queries, delay: 200 * time.Millisecond}
	withGovernanceObserverWriteBudget(t, &governanceFakeProvider{}, slowStore, 1*time.Millisecond)

	w, commentID := createCommentForGovernanceTest(t, issueID, "identical content across all four cases")
	if w.Code != baselineW.Code {
		t.Fatalf("status code = %d, want %d (baseline) — a slow background write must never change the comment response", w.Code, baselineW.Code)
	}
	got := normalizedCommentResponse(t, w.Body.Bytes())
	assertNormalizedResponsesEqual(t, got, baseline)

	testHandler.GovernanceReceipts.WaitForIdle()
	if got := governanceReceiptCountForComment(t, commentID); got != 0 {
		t.Errorf("governance_receipt rows = %d, want 0 (1ms WriteBudget against a 200ms-delayed store must time out deterministically)", got)
	}
	stats := testHandler.GovernanceReceipts.Stats()
	if stats.PersistFailedTotal < 1 {
		t.Errorf("Stats().PersistFailedTotal = %d, want >= 1 (the timed-out write must be counted)", stats.PersistFailedTotal)
	}
}

// TestCreateComment_GovernanceOn_StatsCountEligibleAndPersisted is CHE-685
// review B3's counters requirement: an eligible observation that reaches a
// decided, persisted receipt must be visible on Observer.Stats() without
// requiring any external metrics system.
func TestCreateComment_GovernanceOn_StatsCountEligibleAndPersisted(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	issueID := createCommentTriggerPreviewIssue(t, "governance stats counters", "member", testUserID)

	withGovernanceFlag(t, true)
	withGovernanceObserver(t, &governanceFakeProvider{}, testHandler.Queries)

	before := testHandler.GovernanceReceipts.Stats()
	w, commentID := createCommentForGovernanceTest(t, issueID, "stats counters case")
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateComment: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	testHandler.GovernanceReceipts.WaitForIdle()
	if got := governanceReceiptCountForComment(t, commentID); got != 1 {
		t.Fatalf("governance_receipt rows = %d, want 1", got)
	}

	after := testHandler.GovernanceReceipts.Stats()
	if after.EligibleTotal != before.EligibleTotal+1 {
		t.Errorf("EligibleTotal = %d, want %d", after.EligibleTotal, before.EligibleTotal+1)
	}
	if after.PersistedTotal != before.PersistedTotal+1 {
		t.Errorf("PersistedTotal = %d, want %d", after.PersistedTotal, before.PersistedTotal+1)
	}
}

// ---- native mention-routing behavior unaffected --------------------------

// TestCreateComment_GovernanceOn_NativeMentionRoutingUnaffected proves the
// hard requirement that adding governance observation never changes
// triggerTasksForComment's own routing decisions: an @mention still queues
// exactly one task for the mentioned agent whether or not governance
// observation is enabled.
func TestCreateComment_GovernanceOn_NativeMentionRoutingUnaffected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "Governance Routing Agent", nil)
	issueID := createCommentTriggerPreviewIssue(t, "governance native routing", "member", testUserID)

	withGovernanceFlag(t, true)
	withGovernanceObserver(t, &governanceFakeProvider{}, testHandler.Queries)

	content := "[@Agent](mention://agent/" + agentID + ") please take a look"
	w, commentID := createCommentForGovernanceTest(t, issueID, content)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateComment: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if got := countQueuedCommentTriggerTasks(t, issueID, agentID); got != 1 {
		t.Errorf("queued tasks for mentioned agent = %d, want 1 (governance observation must not change native routing)", got)
	}
	testHandler.GovernanceReceipts.WaitForIdle()
	if got := governanceReceiptCountForComment(t, commentID); got != 1 {
		t.Errorf("governance_receipt rows = %d, want 1", got)
	}
}
