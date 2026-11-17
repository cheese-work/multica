package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-6720: a busy_retry answer names the step that refused, so the next
// occurrence is diagnosable without the server log. One case per lock an
// approval of a weekly run guard card can meet.
func TestApproveRunGuardCardBusyRetryBodyNamesTheRefusingStep(test *testing.T) {
	if testHandler == nil {
		test.Skip("database not available")
	}
	delays := decisionActionRetryDelays
	decisionActionRetryDelays = []time.Duration{time.Millisecond}
	test.Cleanup(func() { decisionActionRetryDelays = delays })
	const request = `(SELECT decision_request_id FROM decision_action WHERE id = $1)`
	for _, held := range []struct{ step, query string }{
		{"chat_session", `SELECT 1 FROM chat_session WHERE id = (SELECT chat_session_id FROM decision_request WHERE id = ` + request + `) FOR UPDATE`},
		{"agents", holdAskingAgent},
		{"issue", `SELECT 1 FROM issue WHERE id = (SELECT issue_id FROM decision_request WHERE id = ` + request + `) FOR UPDATE`},
		{"request", `SELECT 1 FROM decision_request WHERE id = ` + request + ` FOR UPDATE`},
		{"executor", `SELECT 1 FROM weekly_run_guard_refusal WHERE action_id IS NULL AND issue_id = (SELECT issue_id FROM decision_request WHERE id = ` + request + `) FOR UPDATE`},
	} {
		test.Run(held.step, func(test *testing.T) {
			c, _ := weeklyCardCase(test, 6720100)
			action := c.pendingAllow(test)
			holdRow(test, held.query, action.actionID)
			verify := func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) { return pgtype.UUID{}, nil }
			body := action.approve(test, withVerifier(verify), action.claim(test, map[string]string{}), http.StatusConflict)
			var answer struct{ Error, Step string }
			if err := json.Unmarshal([]byte(body), &answer); err != nil || answer.Step != held.step {
				test.Fatalf("body %q: step = %q, want %q (%v)", body, answer.Step, held.step, err)
			}
		})
	}
}
