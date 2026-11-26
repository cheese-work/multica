package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5754: a plain agent ask whose class is reversible asks a person for work
// Standing Delivery Authorization already allows. Block mode refuses it; off and
// warn let it through, and a class SDA escalates is never touched.
func TestDecisionRequestReversibleAskGuard(test *testing.T) {
	for _, tc := range []struct {
		name, mode, class string
		want              int
		created           int
	}{
		{"block refuses reversible", "block", "reversible", http.StatusUnprocessableEntity, 0},
		{"block allows an escalation class", "block", "spend", http.StatusCreated, 1},
		{"warn allows reversible", "warn", "reversible", http.StatusCreated, 1},
		{"off allows reversible", "off", "reversible", http.StatusCreated, 1},
	} {
		test.Run(tc.name, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			setReversibleAskGuard(test, tc.mode)
			request := newRequest(http.MethodPost, "/api/decisions", map[string]any{
				"issue_id": fixture.issueID, "session_id": fixture.sessionID,
				"ask": fixture.structuredAsk(), "escalation_class": tc.class,
			})
			request.Header.Set("X-Actor-Source", "task_token")
			request.Header.Set("X-Agent-ID", fixture.agentID)
			request.Header.Set("X-Task-ID", fixture.taskID)
			testutil.Call(test, testHandler.RequestDecision, request).Want(tc.want)
			fixture.assertCounts(test, tc.created)
		})
	}
}

// A Discovery brief card (Plan step 13) is class reversible by contract. The typed
// subject discovery_brief is the only exemption; the same card with another subject
// or none stays refused, and the class is not rewritten.
func TestDecisionRequestReversibleGuardExemptsDiscoveryBrief(test *testing.T) {
	for _, tc := range []struct {
		name, subject string
		want, created int
	}{
		{"discovery_brief passes in block mode", "discovery_brief", http.StatusCreated, 1},
		{"no subject stays refused", "", http.StatusUnprocessableEntity, 0},
	} {
		test.Run(tc.name, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			setReversibleAskGuard(test, "block")
			ask := fixture.structuredAsk()
			ask.Subject = tc.subject
			testutil.Call(test, testHandler.RequestDecision, fixture.artifactRequest(ask)).Want(tc.want)
			fixture.assertCounts(test, tc.created)
		})
	}
}

func setReversibleAskGuard(t *testing.T, mode string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE workspace SET settings = jsonb_set(COALESCE(settings, '{}'), '{reversible_ask_guard}', to_jsonb($1::text)) WHERE id = $2`,
		mode, testWorkspaceID); err != nil {
		t.Fatalf("set mode: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings = settings - 'reversible_ask_guard' WHERE id = $1`, testWorkspaceID)
	})
}
