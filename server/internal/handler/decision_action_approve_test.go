package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const secretSentinel = "s3cr3t-sentinel-value"

// newPendingSecretAction asks for env.patch with one secret slot named KEY.
func newPendingSecretAction(test *testing.T) pendingAction {
	test.Helper()
	fixture := newActionFixture(test)
	body := fixture.withAction(map[string]any{
		"type": "agent.env.patch", "version": 1,
		"params":       map[string]any{"agent_id": fixture.agentID, "set": map[string]string{"A": "1"}},
		"secret_slots": []map[string]any{{"slot": "KEY", "target": map[string]string{"agent_id": fixture.agentID, "env_key": "KEY"}}},
	})
	var created actionResponse
	fixture.respond(test, body, http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	action := pendingAction{fixture, created.ID, created.Action.ID}
	action.setEnvAuthority(test, true)
	return action
}

func (action pendingAction) hash(test *testing.T) string {
	test.Helper()
	var hash string
	dbfx.QueryRow(test, `SELECT payload_sha256 FROM decision_action WHERE id = $1`, action.actionID).Scan(&hash)
	return hash
}

func (action pendingAction) approve(test *testing.T, handler *Handler, body map[string]any, status int) string {
	test.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		test.Fatal(err)
	}
	request := action.decide("approve", action.humanID, "interactive_jwt")
	request.Body = readerCloser{bytes.NewReader(raw)}
	return testutil.Call(test, handler.ApproveDecisionAction, request).Want(status).Text()
}

type readerCloser struct{ *bytes.Reader }

func (readerCloser) Close() error { return nil }

func (action pendingAction) claim(test *testing.T, secrets map[string]string) map[string]any {
	return map[string]any{"payload_sha256": action.hash(test), "assertion": map[string]any{"id": "fake"}, "secrets": secrets}
}

func withVerifier(verify ApprovalVerifier) *Handler {
	copied := *testHandler
	copied.ApprovalVerifier = verify
	return &copied
}

func acceptingVerifier(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) {
	return pgtype.UUID{}, nil
}

func (action pendingAction) agentEnvVersion(test *testing.T) (version int, env string) {
	test.Helper()
	dbfx.QueryRow(test, `SELECT custom_env_version, custom_env::text FROM agent WHERE id = $1`, action.agentID).Scan(&version, &env)
	return version, env
}

func TestApproveDecisionActionFailsClosedWithoutAVerifier(test *testing.T) {
	action := newPendingAction(test)
	before, _ := action.agentEnvVersion(test)
	body := action.approve(test, testHandler, action.claim(test, nil), http.StatusForbidden)
	if !strings.Contains(body, "approval_proof_unavailable") {
		test.Fatalf("body %q lacks approval_proof_unavailable", body)
	}
	action.assertUntouched(test)
	if after, _ := action.agentEnvVersion(test); after != before {
		test.Fatal("agent env changed without a verified approval")
	}
}

func TestApproveDecisionActionExecutesOnceAndKeepsTheSecretOut(test *testing.T) {
	action := newPendingSecretAction(test)
	before, _ := action.agentEnvVersion(test)
	handler := withVerifier(acceptingVerifier)
	var out decisionActionResponse
	decodeJSON(test, action.approve(test, handler, action.claim(test, map[string]string{"KEY": secretSentinel}), http.StatusOK), &out)
	if out.State != "executed" {
		test.Fatalf("response state = %s", out.State)
	}
	after, env := action.agentEnvVersion(test)
	if after != before+1 || !strings.Contains(env, `"A": "1"`) || !strings.Contains(env, secretSentinel) {
		test.Fatalf("agent env version %d->%d, env %s", before, after, env)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_action WHERE id = $1 AND state = 'executed' AND approved_by_user_id = $2 AND executed_at IS NOT NULL AND post_commit_done_at IS NOT NULL`, action.actionID, action.humanID); n != 1 {
		test.Fatal("action row is not a finished, approved, post-commit-done execution")
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'answered'`, action.requestID); n != 1 {
		test.Fatal("request is not answered")
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1 AND answer_source = 'card' AND wake_at IS NOT NULL`, action.requestID); n != 1 {
		test.Fatal("no scheduled card answer")
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system' AND decision_comment_kind = 'quote' AND content LIKE '%executed%'`, action.issueID); n != 1 {
		test.Fatal("no system comment about the execution")
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE action = 'agent_env_updated' AND details->>'decision_action_id' = $1 AND actor_id = $2`, action.actionID, action.humanID); n != 1 {
		test.Fatal("no env audit row naming the action and its approver")
	}
	for _, table := range []string{"comment", "activity_log", "decision_action", "decision_answer", "decision_request", "agent_task_queue", "chat_message"} {
		if n := dbfx.Count(test, `SELECT count(*) FROM `+table+` t WHERE to_jsonb(t)::text LIKE '%`+secretSentinel+`%'`); n != 0 {
			test.Fatalf("secret value found in %s", table)
		}
	}
	action.approve(test, handler, action.claim(test, map[string]string{"KEY": secretSentinel}), http.StatusConflict)
	if again, _ := action.agentEnvVersion(test); again != after {
		test.Fatal("a second approval executed again")
	}
}

func TestApproveDecisionActionRefusals(test *testing.T) {
	handler := withVerifier(acceptingVerifier)
	for _, tc := range []struct {
		name     string
		prepare  func(action pendingAction)
		claim    func(action pendingAction) map[string]any
		status   int
		wantCode string
	}{
		{"payload hash mismatch", nil, func(action pendingAction) map[string]any {
			claim := action.claim(test, map[string]string{"KEY": secretSentinel})
			claim["payload_sha256"] = strings.Repeat("0", 64)
			return claim
		}, http.StatusConflict, "payload_mismatch"},
		{"precondition drift", func(action pendingAction) {
			dbfx.Exec(test, `UPDATE agent SET custom_env_version = custom_env_version + 1 WHERE id = $1`, action.agentID)
		}, nil, http.StatusConflict, "precondition_drift"},
		{"expired", func(action pendingAction) {
			dbfx.Exec(test, `UPDATE decision_action SET expires_at = clock_timestamp() - interval '1 minute' WHERE id = $1`, action.actionID)
		}, nil, http.StatusConflict, "expired"},
		{"issue closed before the sweeper ran", func(action pendingAction) {
			dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, action.issueID)
		}, nil, http.StatusConflict, "not_pending"},
		{"missing secret", nil, func(action pendingAction) map[string]any { return action.claim(test, nil) }, http.StatusBadRequest, "secrets_mismatch"},
		{"unexpected secret", nil, func(action pendingAction) map[string]any {
			return action.claim(test, map[string]string{"KEY": secretSentinel, "OTHER": "x"})
		}, http.StatusBadRequest, "secrets_mismatch"},
		{"masked marker as secret", nil, func(action pendingAction) map[string]any {
			return action.claim(test, map[string]string{"KEY": "****"})
		}, http.StatusBadRequest, "secrets_mismatch"},
	} {
		test.Run(tc.name, func(test *testing.T) {
			action := newPendingSecretAction(test)
			if tc.prepare != nil {
				tc.prepare(action)
			}
			claim := action.claim(test, map[string]string{"KEY": secretSentinel})
			if tc.claim != nil {
				claim = tc.claim(action)
			}
			before, _ := action.agentEnvVersion(test)
			body := action.approve(test, handler, claim, tc.status)
			if !strings.Contains(body, tc.wantCode) {
				test.Fatalf("body %q lacks %q", body, tc.wantCode)
			}
			action.assertUntouched(test)
			if n := dbfx.Count(test, `SELECT count(*) FROM agent WHERE id = $1 AND custom_env::text LIKE '%`+secretSentinel+`%'`, action.agentID); n != 0 {
				test.Fatal("the secret was written despite the refusal")
			}
			if after, _ := action.agentEnvVersion(test); after < before {
				test.Fatal("env version went backwards")
			}
		})
	}
}

func TestApproveDecisionActionRefusedByTheVerifierWritesNothing(test *testing.T) {
	action := newPendingSecretAction(test)
	refuse := func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) {
		return pgtype.UUID{}, decisionIntentRefusal{http.StatusForbidden, "assertion_invalid: not signed by an enrolled passkey"}
	}
	body := action.approve(test, withVerifier(refuse), action.claim(test, map[string]string{"KEY": secretSentinel}), http.StatusForbidden)
	if !strings.Contains(body, "assertion_invalid") {
		test.Fatalf("body %q lacks assertion_invalid", body)
	}
	action.assertUntouched(test)
}

func TestApproveDecisionActionRefusesNonHumanCallers(test *testing.T) {
	action := newPendingSecretAction(test)
	request := action.decide("approve", action.humanID, "task_token")
	testutil.Call(test, withVerifier(acceptingVerifier).ApproveDecisionAction, request).Want(http.StatusForbidden)
	action.assertUntouched(test)
}

func TestApproveDecisionActionFailureRollsBackTheExecution(test *testing.T) {
	action := newPendingSecretAction(test)
	restore := decisionActionExecutors["agent.env.patch"]
	decisionActionExecutors["agent.env.patch"] = func(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
		if _, err := restore(ctx, execution); err != nil {
			return nil, err
		}
		return nil, actionFailure{Code: "boom", Message: "the change was refused after it was applied"}
	}
	test.Cleanup(func() { decisionActionExecutors["agent.env.patch"] = restore })
	before, envBefore := action.agentEnvVersion(test)
	var out decisionActionOutcome
	decodeJSON(test, action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{"KEY": secretSentinel}), http.StatusOK), &out)
	if out.State != "failed" || out.ErrorCode != "boom" {
		test.Fatalf("outcome = %+v", out)
	}
	if after, env := action.agentEnvVersion(test); after != before || env != envBefore {
		test.Fatalf("savepoint did not undo the execution: version %d->%d env %s", before, after, env)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE action = 'agent_env_updated' AND details->>'decision_action_id' = $1`, action.actionID); n != 0 {
		test.Fatal("the rolled-back execution left an audit row")
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'answered'`, action.requestID); n != 1 {
		test.Fatal("a failed execution must still answer the request")
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system' AND decision_comment_kind = 'quote' AND content LIKE '%failed%'`, action.issueID); n != 1 {
		test.Fatal("no system comment about the failure")
	}
}

func TestApproveDecisionActionNeverWaitsOnAHeldRow(test *testing.T) {
	for _, held := range []struct{ name, query string }{
		{"action", `SELECT 1 FROM decision_action WHERE id = $1 FOR UPDATE`},
		{"target agent", `SELECT 1 FROM agent WHERE id = (SELECT requested_by_agent_id FROM decision_action WHERE id = $1) FOR UPDATE`},
		{"issue", `SELECT 1 FROM issue WHERE id = (SELECT issue_id FROM decision_request WHERE id = (SELECT decision_request_id FROM decision_action WHERE id = $1)) FOR UPDATE`},
	} {
		test.Run(held.name, func(test *testing.T) {
			action := newPendingSecretAction(test)
			holdRow(test, held.query, action.actionID)
			started := time.Now()
			body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{"KEY": secretSentinel}), http.StatusConflict)
			if !strings.Contains(body, "busy_retry") || time.Since(started) > time.Second {
				test.Fatalf("after %s body %q; want an immediate busy_retry", time.Since(started), body)
			}
			action.assertUntouched(test)
		})
	}
}

func TestApproveDecisionActionTakesOnlyNowaitLocks(test *testing.T) {
	action := newPendingSecretAction(test)
	tracker := &lockTracker{}
	handler := handlerWith(tracker)
	handler.ApprovalVerifier = acceptingVerifier
	action.approve(test, handler, action.claim(test, map[string]string{"KEY": secretSentinel}), http.StatusOK)
	if len(tracker.locks) < 5 {
		test.Fatalf("tracker saw locks %v; the sequence is not being recorded", tracker.locks)
	}
	if len(tracker.fails) > 0 {
		test.Fatalf("lock rule violations:\n%s", strings.Join(tracker.fails, "\n"))
	}
}

func TestApproveDecisionActionUncertainCommitReadsTheTrueState(test *testing.T) {
	lost := errors.New("connection reset during commit")
	action := newPendingSecretAction(test)
	tracker := &lockTracker{commit: func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return lost
	}}
	handler := handlerWith(tracker)
	handler.ApprovalVerifier = acceptingVerifier
	var out decisionActionResponse
	decodeJSON(test, action.approve(test, handler, action.claim(test, map[string]string{"KEY": secretSentinel}), http.StatusOK), &out)
	if out.State != "executed" {
		test.Fatalf("state = %s, want executed", out.State)
	}
}

// The asking agent is archived but the sweeper has not cancelled the request
// yet: approval must not write the target's env.
func TestApproveDecisionActionRefusesAnArchivedAskingAgentBeforeTheSweeper(test *testing.T) {
	fixture := newActionFixture(test)
	targetID := createHandlerTestAgent(test, "approve archived-asker target", []byte("[]"))
	var created actionResponse
	fixture.respond(test, fixture.withAction(fixture.envPatch(map[string]any{"agent_id": targetID, "set": map[string]string{"A": "1"}})), http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	action := pendingAction{fixture, created.ID, created.Action.ID}
	dbfx.Exec(test, `UPDATE agent SET archived_at = clock_timestamp() WHERE id = $1`, fixture.agentID)
	body := action.approve(test, withVerifier(acceptingVerifier), map[string]any{"payload_sha256": action.hash(test), "assertion": map[string]any{"id": "fake"}}, http.StatusConflict)
	if !strings.Contains(body, "not_pending") {
		test.Fatalf("body %q lacks not_pending", body)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM agent WHERE id = $1 AND custom_env->>'A' = '1'`, targetID); n != 0 {
		test.Fatal("the target env was written by an approval from an archived asking agent")
	}
}
