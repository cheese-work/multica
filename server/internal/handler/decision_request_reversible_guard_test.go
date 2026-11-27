package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisiontx"
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
// subject discovery_brief exempts it only when the issue follows the Plan (gstack)
// workflow and the ask has the brief shape: two options, "Tiếp tục" and "Dừng", and no
// artifacts. The same label on an ordinary issue, a relabelled ordinary approval, or no
// label stays refused, and the class is not rewritten.
func TestDecisionRequestReversibleGuardExemptsDiscoveryBrief(test *testing.T) {
	for _, tc := range []struct {
		name, subject, workflow string
		brief                   bool
		want, created           int
	}{
		{"discovery_brief on a Plan (gstack) issue passes in block mode", "discovery_brief", "Plan (gstack)", true, http.StatusCreated, 1},
		{"discovery_brief on an ordinary issue stays refused", "discovery_brief", "", true, http.StatusUnprocessableEntity, 0},
		{"discovery_brief on another workflow stays refused", "discovery_brief", "Default B", true, http.StatusUnprocessableEntity, 0},
		{"no subject on a Plan (gstack) issue stays refused", "", "Plan (gstack)", true, http.StatusUnprocessableEntity, 0},
		{"relabelled ordinary approval on a Plan (gstack) issue stays refused", "discovery_brief", "Plan (gstack)", false, http.StatusUnprocessableEntity, 0},
	} {
		test.Run(tc.name, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			setReversibleAskGuard(test, "block")
			if tc.workflow != "" {
				setIssueWorkflowByName(test, fixture.issueID, tc.workflow)
			}
			ask := fixture.structuredAsk()
			if tc.brief {
				ask = briefOptions(ask)
			}
			ask.Subject = tc.subject
			testutil.Call(test, testHandler.RequestDecision, fixture.artifactRequest(ask)).Want(tc.want)
			fixture.assertCounts(test, tc.created)
		})
	}
}

// briefOptions gives the ask the Discovery brief answer buttons (Plan step 13).
func briefOptions(ask decisiontx.Ask) decisiontx.Ask {
	ask.Options = []decisiontx.AskOption{testAskOption("Tiếp tục"), testAskOption("Dừng")}
	ask.RecommendedOption, ask.Recommendation = "Tiếp tục", "Tiếp tục"
	return ask
}

// setIssueWorkflowByName gives the issue its own workflow row of that name. The row
// and the override go away with the test.
func setIssueWorkflowByName(t *testing.T, issueID, name string) {
	t.Helper()
	ctx := context.Background()
	var workflowID string
	if err := testPool.QueryRow(ctx, `INSERT INTO issue_workflow (workspace_id, name, description, initial_status_key, steps, owner_id) VALUES ($1, $2, '', 'todo', '[]'::jsonb, $3) RETURNING id`, testWorkspaceID, name, testUserID).Scan(&workflowID); err != nil {
		t.Fatalf("insert workflow: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE issue SET workflow_id = $1 WHERE id = $2`, workflowID, issueID); err != nil {
		t.Fatalf("set issue workflow: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE issue SET workflow_id = NULL WHERE id = $1`, issueID)
		testPool.Exec(ctx, `DELETE FROM issue_workflow WHERE id = $1`, workflowID)
	})
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
