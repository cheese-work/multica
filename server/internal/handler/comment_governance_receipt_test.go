package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/governance"
	"github.com/multica-ai/multica/server/internal/governance/receipt"
	"github.com/multica-ai/multica/server/internal/util"
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
	if enabled {
		useCompleteGovernanceConfigFixture(t)
	}
	previous := testHandler.FeatureFlags
	sp := featureflag.NewStaticProvider()
	sp.LoadRules(map[string]featureflag.Rule{featureflags.JevReceipts: {Default: enabled}})
	testHandler.FeatureFlags = featureflag.NewService(sp)
	t.Cleanup(func() { testHandler.FeatureFlags = previous })
}

type blockingGovernanceProvider struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

type observedTxStarter struct {
	inner   txStarter
	started chan<- int32
}

func (starter observedTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := starter.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	var pid int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		_ = tx.Rollback(context.Background())
		return nil, err
	}
	starter.started <- pid
	return tx, nil
}

func (provider *blockingGovernanceProvider) Evaluate(ctx context.Context, request jev.Request) (*jev.Response, error) {
	if provider.calls.Add(1) == 1 {
		close(provider.entered)
		select {
		case <-provider.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return (&governanceFakeProvider{}).Evaluate(ctx, request)
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

func TestGovernanceDisableWaitsForInFlightEvaluationAdmission(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	requireGovernanceConfigTables(t)
	issueID := createCommentTriggerPreviewIssue(t, "governance disable race", "member", testUserID)
	withGovernanceFlag(t, true)
	provider := &blockingGovernanceProvider{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-provider.release:
		default:
			close(provider.release)
		}
	})
	withGovernanceObserver(t, provider, testHandler.Queries)
	recorder := httptest.NewRecorder()
	request := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": "observe while disable races"}), "id", issueID)
	observed := make(chan struct{})
	go func() {
		testHandler.CreateComment(recorder, request)
		close(observed)
	}()
	select {
	case <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("governance evaluation did not start")
	}
	workspaceID, err := util.ParseUUID(testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	actorID, err := util.ParseUUID(testUserID)
	if err != nil {
		t.Fatal(err)
	}
	var lockedWorkspaceID string
	lockErr := testPool.QueryRow(context.Background(), `
		SELECT workspace_id
		FROM governance_workspace_config
		WHERE workspace_id = $1
		FOR UPDATE NOWAIT
	`, workspaceID).Scan(&lockedWorkspaceID)
	var postgresErr *pgconn.PgError
	if !errors.As(lockErr, &postgresErr) || postgresErr.Code != "55P03" {
		t.Fatalf("in-flight evaluation control lock error = %v, want PostgreSQL lock-not-available", lockErr)
	}
	originalStarter := testHandler.TxStarter
	disablePIDs := make(chan int32, 1)
	testHandler.TxStarter = observedTxStarter{inner: originalStarter, started: disablePIDs}
	t.Cleanup(func() { testHandler.TxStarter = originalStarter })
	expectedVersion := int64(1)
	disabled := false
	requestID := uuid.New()
	disableDone := make(chan error, 1)
	go func() {
		_, writeErr := testHandler.writeGovernanceConfig(context.Background(), workspaceID, actorID, requestID, strings.Repeat("0", 64), governanceConfigPatch{
			RequestID: requestID.String(), ExpectedVersion: &expectedVersion, JevGovernanceEnabled: &disabled,
		})
		disableDone <- writeErr
	}()
	var disablePID int32
	select {
	case disablePID = <-disablePIDs:
	case writeErr := <-disableDone:
		close(provider.release)
		t.Fatalf("master disable returned before exposing its backend: %v", writeErr)
	case <-time.After(5 * time.Second):
		close(provider.release)
		t.Fatal("master disable transaction did not begin")
	}
	if err := waitForGovernanceBackendLockWait(t, context.Background(), disablePID, 5*time.Second); err != nil {
		close(provider.release)
		t.Fatalf("disable did not wait for the admitted evaluation: %v", err)
	}
	select {
	case writeErr := <-disableDone:
		close(provider.release)
		t.Fatalf("master disable completed during admitted evaluation: %v", writeErr)
	default:
	}
	close(provider.release)
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("comment observation did not finish")
	}
	if recorder.Code != http.StatusCreated {
		t.Fatalf("comment response = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response CommentResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.ID == "" {
		t.Fatalf("decode observed comment response id=%q err=%v: %s", response.ID, err, recorder.Body.String())
	}
	if err := <-disableDone; err != nil {
		t.Fatalf("master disable: %v", err)
	}
	testHandler.TxStarter = originalStarter
	testHandler.GovernanceReceipts.WaitForIdle()
	config, err := loadGovernanceControl(context.Background(), testPool, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if config.Settings.JevGovernanceEnabled || config.ControlEpoch != 2 {
		t.Fatalf("disabled config = enabled:%v epoch:%d, want false/2", config.Settings.JevGovernanceEnabled, config.ControlEpoch)
	}
	second := httptest.NewRecorder()
	secondRequest := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": "after disable acknowledgement"}), "id", issueID)
	testHandler.CreateComment(second, secondRequest)
	if second.Code != http.StatusCreated {
		t.Fatalf("post-disable comment = %d: %s", second.Code, second.Body.String())
	}
	var secondResponse CommentResponse
	if err := json.Unmarshal(second.Body.Bytes(), &secondResponse); err != nil {
		t.Fatalf("decode post-disable comment: %v", err)
	}
	testHandler.GovernanceReceipts.WaitForIdle()
	if got := governanceReceiptCountForComment(t, secondResponse.ID); got != 0 {
		t.Fatalf("post-disable governance receipts = %d, want 0", got)
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("provider calls = %d after acknowledged disable, want 1 total", calls)
	}
}

func TestGovernanceDisableWinsConcurrentEvaluationAdmission(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	requireGovernanceConfigTables(t)
	issueID := createCommentTriggerPreviewIssue(t, "governance disable wins race", "member", testUserID)
	withGovernanceFlag(t, true)
	provider := &blockingGovernanceProvider{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-provider.release:
		default:
			close(provider.release)
		}
	})
	withGovernanceObserver(t, provider, testHandler.Queries)
	workspaceID, err := util.ParseUUID(testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	disableConn, err := pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer disableConn.Close(context.Background())
	disableTx, err := disableConn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = disableTx.Rollback(context.Background())
		}
	})
	if _, err := disableTx.Exec(ctx, `
		UPDATE governance_workspace_config
		SET config_version = config_version + 1,
		    control_epoch = control_epoch + 1,
		    settings = jsonb_set(settings, '{jev_governance_enabled}', 'false'::jsonb, true),
		    updated_at = now()
		WHERE workspace_id = $1
	`, workspaceID); err != nil {
		t.Fatal(err)
	}
	var disablePID int32
	if err := disableConn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&disablePID); err != nil {
		t.Fatal(err)
	}
	originalStarter := testHandler.TxStarter
	started := make(chan int32, 8)
	testHandler.TxStarter = observedTxStarter{inner: originalStarter, started: started}
	t.Cleanup(func() { testHandler.TxStarter = originalStarter })
	recorder := httptest.NewRecorder()
	request := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": "disable wins admission race"}), "id", issueID)
	requestDone := make(chan struct{})
	go func() {
		testHandler.CreateComment(recorder, request)
		close(requestDone)
	}()
	var writerPID int32
	select {
	case writerPID = <-started:
	case <-provider.entered:
		t.Fatal("evaluation reached the provider before disable committed")
	case <-requestDone:
		t.Fatal("comment request completed before evaluation admission waited on disable")
	case <-ctx.Done():
		t.Fatalf("evaluation admission did not begin before timeout: %v", ctx.Err())
	}
	if writerPID == disablePID {
		t.Fatalf("evaluation writer reused disable backend pid %d", disablePID)
	}
	if err := waitForGovernanceBackendLockWait(t, ctx, writerPID, 5*time.Second); err != nil {
		t.Fatalf("evaluation admission did not wait on disable: %v", err)
	}
	if err := disableTx.Commit(ctx); err != nil {
		t.Fatalf("commit disable before evaluation admission: %v", err)
	}
	committed = true
	select {
	case <-requestDone:
	case <-provider.entered:
		t.Fatal("evaluation reached the provider after disable commit")
	case <-ctx.Done():
		t.Fatalf("comment request did not complete after disable: %v", ctx.Err())
	}
	if recorder.Code != http.StatusCreated {
		t.Fatalf("comment response = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response CommentResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.ID == "" {
		t.Fatalf("decode comment response id=%q err=%v: %s", response.ID, err, recorder.Body.String())
	}
	testHandler.GovernanceReceipts.WaitForIdle()
	if got := governanceReceiptCountForComment(t, response.ID); got != 0 {
		t.Fatalf("disabled evaluation persisted %d receipt rows, want none", got)
	}
	if calls := provider.calls.Load(); calls != 0 {
		t.Fatalf("provider calls after disable won = %d, want 0", calls)
	}
	config, err := loadGovernanceControl(context.Background(), testPool, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if config.Settings.JevGovernanceEnabled || config.ControlEpoch != 2 {
		t.Fatalf("disable-wins config = enabled:%v epoch:%d, want false/2", config.Settings.JevGovernanceEnabled, config.ControlEpoch)
	}
}

func waitForGovernanceBackendLockWait(t *testing.T, ctx context.Context, pid int32, timeout time.Duration) error {
	t.Helper()
	observer, err := pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer observer.Close(context.Background())
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := observer.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1)) > 0`, pid).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("backend did not enter a PostgreSQL lock wait")
		case <-ticker.C:
		}
	}
}

func TestGovernanceReceiptPersistenceRacesDisable(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	requireGovernanceConfigTables(t)
	for _, order := range []string{"receipt first", "disable first"} {
		t.Run(order, func(t *testing.T) {
			withGovernanceFlag(t, false)
			issueID := createCommentTriggerPreviewIssue(t, "governance receipt persistence race", "member", testUserID)
			comment, commentID := createCommentForGovernanceTest(t, issueID, "receipt persistence race input")
			if comment.Code != http.StatusCreated || commentID == "" {
				t.Fatalf("create receipt source comment = %d id=%q: %s", comment.Code, commentID, comment.Body.String())
			}
			withGovernanceFlag(t, true)
			config, err := loadGovernanceControl(context.Background(), testPool, parseUUID(testWorkspaceID))
			if err != nil {
				t.Fatal(err)
			}
			params := db.InsertGovernanceReceiptParams{
				WorkspaceID: parseUUID(testWorkspaceID), IssueID: parseUUID(issueID), CommentID: parseUUID(commentID),
				Trigger: string(receipt.TriggerCreate), Status: "decided", Answers: []byte("[]"),
				ObservedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}, ControlEpoch: config.ControlEpoch,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			receiptConn, err := pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			defer receiptConn.Close(context.Background())
			disableConn, err := pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			defer disableConn.Close(context.Background())

			if order == "receipt first" {
				writerTx, err := receiptConn.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer writerTx.Rollback(context.Background())
				stored, err := db.New(writerTx).InsertGovernanceReceipt(ctx, params)
				if err != nil {
					t.Fatalf("persist receipt before disable: %v", err)
				}
				started, disabled := startGovernanceControlDisable(ctx, disableConn, params.WorkspaceID)
				var disablePID int32
				select {
				case disablePID = <-started:
				case err := <-disabled:
					t.Fatalf("disable failed before lock observation: %v", err)
				case <-ctx.Done():
					t.Fatalf("disable did not start: %v", ctx.Err())
				}
				if err := waitForGovernanceBackendLockWait(t, ctx, disablePID, 5*time.Second); err != nil {
					t.Fatalf("disable did not wait for receipt transaction: %v", err)
				}
				select {
				case err := <-disabled:
					t.Fatalf("disable completed before receipt commit: %v", err)
				default:
				}
				if err := writerTx.Commit(ctx); err != nil {
					t.Fatalf("commit receipt before disable: %v", err)
				}
				if err := <-disabled; err != nil {
					t.Fatalf("disable after receipt commit: %v", err)
				}
				var storedEpoch int64
				if err := testPool.QueryRow(ctx, `SELECT control_epoch FROM governance_receipt WHERE id = $1`, stored.ID).Scan(&storedEpoch); err != nil {
					t.Fatalf("read receipt after disable: %v", err)
				}
				if storedEpoch != config.ControlEpoch {
					t.Fatalf("stored receipt epoch = %d, want captured %d", storedEpoch, config.ControlEpoch)
				}
				return
			}

			disableTx, err := disableConn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer disableTx.Rollback(context.Background())
			if _, err := disableTx.Exec(ctx, `
				UPDATE governance_workspace_config
				SET config_version = config_version + 1, control_epoch = control_epoch + 1,
				    settings = jsonb_set(settings, '{jev_governance_enabled}', 'false'::jsonb, true), updated_at = now()
				WHERE workspace_id = $1
			`, params.WorkspaceID); err != nil {
				t.Fatalf("hold disable control lock: %v", err)
			}
			writerTx, err := receiptConn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writerTx.Rollback(context.Background())
			var writerPID int32
			if err := writerTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID); err != nil {
				t.Fatal(err)
			}
			inserted := make(chan error, 1)
			go func() {
				_, insertErr := db.New(writerTx).InsertGovernanceReceipt(ctx, params)
				inserted <- insertErr
			}()
			if err := waitForGovernanceBackendLockWait(t, ctx, writerPID, 5*time.Second); err != nil {
				t.Fatalf("receipt insert did not wait for disable: %v", err)
			}
			if err := disableTx.Commit(ctx); err != nil {
				t.Fatalf("commit disable before receipt insert: %v", err)
			}
			if err := <-inserted; !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("stale receipt after disable = %v, want pgx.ErrNoRows", err)
			}
			if got := governanceReceiptCountForComment(t, commentID); got != 0 {
				t.Fatalf("receipt rows after disable = %d, want zero", got)
			}
		})
	}
}

func startGovernanceControlDisable(ctx context.Context, conn *pgx.Conn, workspaceID pgtype.UUID) (<-chan int32, <-chan error) {
	started := make(chan int32, 1)
	done := make(chan error, 1)
	go func() {
		tx, err := conn.Begin(ctx)
		if err != nil {
			done <- err
			return
		}
		var pid int32
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			_ = tx.Rollback(context.Background())
			done <- err
			return
		}
		started <- pid
		result, err := tx.Exec(ctx, `
			UPDATE governance_workspace_config
			SET config_version = config_version + 1, control_epoch = control_epoch + 1,
			    settings = jsonb_set(settings, '{jev_governance_enabled}', 'false'::jsonb, true), updated_at = now()
			WHERE workspace_id = $1
		`, workspaceID)
		if err == nil && result.RowsAffected() != 1 {
			err = fmt.Errorf("disable updated %d control rows, want 1", result.RowsAffected())
		}
		if err != nil {
			_ = tx.Rollback(context.Background())
			done <- err
			return
		}
		done <- tx.Commit(ctx)
	}()
	return started, done
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
