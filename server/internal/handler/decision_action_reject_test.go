package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// pendingAction is an env.patch request the fixture's human can decide.
type pendingAction struct {
	singleCallFixture
	requestID, actionID string
}

func newPendingAction(test *testing.T) pendingAction {
	test.Helper()
	fixture := newActionFixture(test)
	body := fixture.withAction(fixture.envPatch(map[string]any{"agent_id": fixture.agentID, "set": map[string]string{"A": "1"}}))
	var created actionResponse
	fixture.respond(test, body, http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	action := pendingAction{fixture, created.ID, created.Action.ID}
	action.setEnvAuthority(test, true)
	return action
}

// setEnvAuthority makes the asked human the agent's owner, or a plain member
// of an agent nobody owns.
func (action pendingAction) setEnvAuthority(test *testing.T, granted bool) {
	test.Helper()
	owner := any(nil)
	if granted {
		owner = action.humanID
	}
	dbfx.Exec(test, `UPDATE agent SET owner_id = $1 WHERE id = $2`, owner, action.agentID)
}

func (action pendingAction) decide(verb, userID, source string) *http.Request {
	request := withURLParam(newRequest(http.MethodPost, "/api/decision-actions/"+action.actionID+"/"+verb, nil), "id", action.actionID)
	request.Header.Set("X-User-ID", userID)
	request.Header.Set("X-Actor-Source", source)
	return request
}

func (action pendingAction) reject(test *testing.T, handler *Handler, status int) string {
	test.Helper()
	return testutil.Call(test, handler.RejectDecisionAction, action.decide("reject", action.humanID, "interactive_jwt")).Want(status).Text()
}

func (action pendingAction) outcome(test *testing.T, handler *Handler, status int) string {
	test.Helper()
	request := withURLParam(newRequest(http.MethodGet, "/api/decision-actions/"+action.actionID+"/outcome", nil), "id", action.actionID)
	request.Header.Set("X-User-ID", action.humanID)
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	return testutil.Call(test, handler.GetDecisionActionOutcome, request).Want(status).Text()
}

func (action pendingAction) state(test *testing.T) string {
	test.Helper()
	var state string
	dbfx.QueryRow(test, `SELECT state FROM decision_action WHERE id = $1`, action.actionID).Scan(&state)
	return state
}

func (action pendingAction) assertUntouched(test *testing.T) {
	test.Helper()
	if state := action.state(test); state != "pending" {
		test.Fatalf("action state = %s, want pending", state)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, action.requestID); n != 0 {
		test.Fatalf("decision_answer rows = %d, want 0", n)
	}
	if status := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, action.requestID); status != 1 {
		test.Fatal("request is no longer open")
	}
}

func decodeJSON(test *testing.T, body string, into any) {
	test.Helper()
	if err := json.Unmarshal([]byte(body), into); err != nil {
		test.Fatalf("body %q: %v", body, err)
	}
}

// holdRow locks one row until the test ends, as a competing writer would.
func holdRow(test *testing.T, query string, args ...any) {
	test.Helper()
	tx, err := testPool.Begin(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(context.Background(), query, args...); err != nil {
		test.Fatal(err)
	}
}

func TestRejectDecisionActionRecordsACardAnswer(test *testing.T) {
	action := newPendingAction(test)
	var out decisionActionResponse
	decodeJSON(test, action.reject(test, testHandler, http.StatusOK), &out)
	if out.State != "rejected" || out.ID != action.actionID {
		test.Fatalf("response = %+v", out)
	}
	if state := action.state(test); state != "rejected" {
		test.Fatalf("stored state = %s", state)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'answered'`, action.requestID); n != 1 {
		test.Fatal("request is not answered")
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1 AND answer_source = 'card' AND answered_by_user_id = $2 AND answered_by_task_id IS NULL AND effective_at <= clock_timestamp()`, action.requestID, action.humanID); n != 1 {
		test.Fatal("no card answer by the request's human")
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'decision_action_rejected' AND actor_type = 'member' AND actor_id = $2`, action.issueID, action.humanID); n != 1 {
		test.Fatalf("decision_action_rejected activity rows = %d, want 1", n)
	}
}

func TestRejectDecisionActionRefusals(test *testing.T) {
	other := dbfx.User(test, "Other member", "other-"+time.Now().Format("150405.000000")+"@test.invalid")
	dbfx.Insert(test, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": other, "role": "owner"})
	for _, tc := range []struct {
		name, user, source string
		status             int
	}{
		{"another member", other, "interactive_jwt", http.StatusForbidden},
		{"task token", "", "task_token", http.StatusForbidden},
		{"cloud pat", "", "cloud_pat", http.StatusForbidden},
	} {
		test.Run(tc.name, func(test *testing.T) {
			action := newPendingAction(test)
			user := tc.user
			if user == "" {
				user = action.humanID
			}
			testutil.Call(test, testHandler.RejectDecisionAction, action.decide("reject", user, tc.source)).Want(tc.status)
			action.assertUntouched(test)
		})
	}
	test.Run("unknown action", func(test *testing.T) {
		action := newPendingAction(test)
		action.actionID = "00000000-0000-4000-8000-000000000002"
		testutil.Call(test, testHandler.RejectDecisionAction, action.decide("reject", action.humanID, "interactive_jwt")).Want(http.StatusNotFound)
	})
}

func TestRejectDecisionActionTwiceIsNotPending(test *testing.T) {
	action := newPendingAction(test)
	action.reject(test, testHandler, http.StatusOK)
	if body := action.reject(test, testHandler, http.StatusConflict); !strings.Contains(body, "not_pending") {
		test.Fatalf("body %q lacks not_pending", body)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, action.requestID); n != 1 {
		test.Fatalf("decision_answer rows = %d, want 1", n)
	}
}

func TestRejectDecisionActionNeverWaitsOnAHeldRow(test *testing.T) {
	for _, held := range []struct{ name, query string }{
		{"action", `SELECT 1 FROM decision_action WHERE id = $1 FOR UPDATE`},
		{"request", `SELECT 1 FROM decision_request WHERE id = (SELECT decision_request_id FROM decision_action WHERE id = $1) FOR UPDATE`},
		{"asking agent", `SELECT 1 FROM agent WHERE id = (SELECT requested_by_agent_id FROM decision_action WHERE id = $1) FOR UPDATE`},
	} {
		test.Run(held.name, func(test *testing.T) {
			action := newPendingAction(test)
			holdRow(test, held.query, action.actionID)
			started := time.Now()
			body := action.reject(test, testHandler, http.StatusConflict)
			if !strings.Contains(body, "busy_retry") || time.Since(started) > time.Second {
				test.Fatalf("after %s body %q; want an immediate busy_retry", time.Since(started), body)
			}
			action.assertUntouched(test)
		})
	}
}

var (
	lockClause = regexp.MustCompile(`(?is)\bFOR\s+(UPDATE|NO\s+KEY\s+UPDATE|SHARE|KEY\s+SHARE)\b(\s+OF\s+\w+)?(\s+NOWAIT)?`)
	fromTable  = regexp.MustCompile(`(?is)\bFROM\s+(\w+)`)
	writeTable = regexp.MustCompile(`(?is)^\s*(?:UPDATE|DELETE\s+FROM)\s+(\w+)`)
	lockRank   = map[string]int{"KEY SHARE": 1, "SHARE": 2, "NO KEY UPDATE": 3, "UPDATE": 4}
)

func sqlBody(text string) string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// lockTracker records every statement of the transactions it starts and
// checks the 3.2 rules: no blocking row or advisory lock, no mode upgrade.
type lockTracker struct {
	inner txStarter
	mu    sync.Mutex
	locks map[string]int
	fails []string
	// commit decides what Commit does: nil commits for real.
	commit func(ctx context.Context, tx pgx.Tx) error
}

func (tracker *lockTracker) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := tracker.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &trackedTx{Tx: tx, tracker: tracker}, nil
}

func (tracker *lockTracker) observe(text string) {
	text = sqlBody(text)
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.locks == nil {
		tracker.locks = map[string]int{}
	}
	if strings.Contains(text, "pg_advisory_xact_lock(") {
		tracker.fails = append(tracker.fails, "blocking advisory lock: "+text)
	}
	if clause := lockClause.FindStringSubmatch(text); clause != nil {
		mode := strings.Join(strings.Fields(strings.ToUpper(clause[1])), " ")
		table := ""
		if from := fromTable.FindStringSubmatch(text); from != nil {
			table = from[1]
			if clause[2] != "" {
				table = strings.TrimSpace(clause[2][strings.LastIndex(clause[2], " ")+1:])
			}
		}
		if clause[3] == "" {
			tracker.fails = append(tracker.fails, "blocking row lock on "+table+": "+text)
		}
		if tracker.locks[table] != 0 && lockRank[mode] > tracker.locks[table] {
			tracker.fails = append(tracker.fails, "mode upgrade on "+table+" to "+mode)
		}
		if lockRank[mode] > tracker.locks[table] {
			tracker.locks[table] = lockRank[mode]
		}
		return
	}
	if write := writeTable.FindStringSubmatch(text); write != nil {
		if held := tracker.locks[write[1]]; held != 0 && held < lockRank["NO KEY UPDATE"] {
			tracker.fails = append(tracker.fails, "write to "+write[1]+" after a weaker lock")
		}
	}
}

type trackedTx struct {
	pgx.Tx
	tracker *lockTracker
}

func (tx *trackedTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tx.tracker.observe(sql)
	return tx.Tx.Exec(ctx, sql, args...)
}

func (tx *trackedTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	tx.tracker.observe(sql)
	return tx.Tx.Query(ctx, sql, args...)
}

func (tx *trackedTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	tx.tracker.observe(sql)
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func (tx *trackedTx) Commit(ctx context.Context) error {
	if tx.tracker.commit != nil {
		return tx.tracker.commit(ctx, tx.Tx)
	}
	return tx.Tx.Commit(ctx)
}

func handlerWith(tracker *lockTracker) *Handler {
	copied := *testHandler
	tracker.inner = testHandler.TxStarter
	copied.TxStarter = tracker
	return &copied
}

func TestRejectDecisionActionTakesOnlyNowaitLocks(test *testing.T) {
	action := newPendingAction(test)
	tracker := &lockTracker{}
	action.reject(test, handlerWith(tracker), http.StatusOK)
	if len(tracker.locks) < 4 {
		test.Fatalf("tracker saw locks %v; the sequence is not being recorded", tracker.locks)
	}
	if len(tracker.fails) > 0 {
		test.Fatalf("lock rule violations:\n%s", strings.Join(tracker.fails, "\n"))
	}
}

func TestRejectDecisionActionUncertainCommitReadsTheTrueState(test *testing.T) {
	lost := errors.New("connection reset during commit")
	for _, tc := range []struct {
		name     string
		commit   func(ctx context.Context, tx pgx.Tx) error
		status   int
		wantCode string
		state    string
	}{
		{"committed then connection lost", func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.Commit(ctx); err != nil {
				return err
			}
			return lost
		}, http.StatusOK, "rejected", "rejected"},
		{"aborted", func(ctx context.Context, tx pgx.Tx) error {
			_ = tx.Rollback(ctx)
			return lost
		}, http.StatusServiceUnavailable, "outcome_unknown_retry", "pending"},
	} {
		test.Run(tc.name, func(test *testing.T) {
			action := newPendingAction(test)
			body := action.reject(test, handlerWith(&lockTracker{commit: tc.commit}), tc.status)
			if !strings.Contains(body, tc.wantCode) {
				test.Fatalf("body %q lacks %q", body, tc.wantCode)
			}
			if state := action.state(test); state != tc.state {
				test.Fatalf("stored state = %s, want %s", state, tc.state)
			}
		})
	}
}

func TestRejectDecisionActionUncertainCommitFailsClosedWhileTheRowIsHeld(test *testing.T) {
	lowerReadbackTimeout(test, 200*time.Millisecond)
	action := newPendingAction(test)
	lost := errors.New("connection reset during commit")
	tracker := &lockTracker{commit: func(ctx context.Context, tx pgx.Tx) error {
		_ = tx.Rollback(ctx)
		// Another transaction now owns the row, as if the original still did.
		holdRow(test, `SELECT 1 FROM decision_action WHERE id = $1 FOR UPDATE`, action.actionID)
		return lost
	}}
	body := action.reject(test, handlerWith(tracker), http.StatusServiceUnavailable)
	if !strings.Contains(body, "outcome_unknown") || strings.Contains(body, "outcome_unknown_retry") {
		test.Fatalf("body %q; want fail-closed outcome_unknown", body)
	}
}

func lowerReadbackTimeout(test *testing.T, timeout time.Duration) {
	test.Helper()
	commit, outcome := commitReadbackTimeout, outcomeReadbackTimeout
	commitReadbackTimeout, outcomeReadbackTimeout = timeout, timeout
	test.Cleanup(func() { commitReadbackTimeout, outcomeReadbackTimeout = commit, outcome })
}

func TestDecisionActionOutcomeReadsUnderTheRowLock(test *testing.T) {
	action := newPendingAction(test)
	var out struct {
		State string `json:"state"`
	}
	decodeJSON(test, action.outcome(test, testHandler, http.StatusOK), &out)
	if out.State != "pending" {
		test.Fatalf("state = %s, want pending", out.State)
	}
	action.reject(test, testHandler, http.StatusOK)
	decodeJSON(test, action.outcome(test, testHandler, http.StatusOK), &out)
	if out.State != "rejected" {
		test.Fatalf("state = %s, want rejected", out.State)
	}
}

func TestDecisionActionOutcomeIsUnknownWhileTheRowIsHeld(test *testing.T) {
	lowerReadbackTimeout(test, 200*time.Millisecond)
	held := newPendingAction(test)
	holdRow(test, `SELECT 1 FROM decision_action WHERE id = $1 FOR UPDATE`, held.actionID)
	if body := held.outcome(test, testHandler, http.StatusServiceUnavailable); !strings.Contains(body, "outcome_unknown") {
		test.Fatalf("body %q lacks outcome_unknown", body)
	}
}

func TestDecisionActionOutcomeIsForTheRequestsHumanOnly(test *testing.T) {
	action := newPendingAction(test)
	request := withURLParam(newRequest(http.MethodGet, "/api/decision-actions/"+action.actionID+"/outcome", nil), "id", action.actionID)
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	testutil.Call(test, testHandler.GetDecisionActionOutcome, request).Want(http.StatusForbidden)
}
