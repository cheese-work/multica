package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

type decisionResumeClaim struct {
	Task *struct {
		ID                    string `json:"id"`
		PriorSessionID        string `json:"prior_session_id"`
		TriggerCommentContent string `json:"trigger_comment_content"`
		TriggerSummary        string `json:"trigger_summary"`
	} `json:"task"`
}

// Answering a decision card (answerDecisionRequestTx) wakes the asking agent on its prior session (CHE-3011).
// The asking run finished with provider session "asking-run-session"; the wake
// task the delivery enqueues must resume it and carry the human's answer.
func TestDecisionDeliveryWakeResumesAskingSession(test *testing.T) {
	for _, recipient := range []string{"assignee", "squad_leader"} {
		test.Run(recipient, func(test *testing.T) {
			resetDecisionLease(test)
			fixture := newCardFixture(test)
			// Claim from a runtime of our own so no unrelated queued task is picked.
			_, runtimeID, daemonID := createRuntimeGuardAgent(test, context.Background())
			dbfx.Exec(test, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, runtimeID, fixture.agentID)
			dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed', runtime_id = $1, session_id = 'asking-run-session', work_dir = '/tmp/asking-run',
				started_at = now() - interval '1 minute', completed_at = now() WHERE id = $2`, runtimeID, fixture.taskID)
			if recipient == "squad_leader" {
				squadID := dbfx.Insert(test, "squad", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Resume squad", "creator_id": testUserID, "leader_id": fixture.agentID})
				dbfx.Exec(test, `UPDATE issue SET assignee_type = 'squad', assignee_id = $1 WHERE id = $2`, squadID, fixture.issueID)
			}

			// The human answers on the card, not in the DM; the asking runs have ended.
			if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option B"}); err != nil {
				test.Fatal(err)
			}
			dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.chatTaskID)
			_, runner := newDecisionRunnerFixture(test)
			decisionPass(test, runner)

			var claim decisionResumeClaim
			claimForRuntimeExpecting(test, testHandler, runtimeID, daemonID, http.StatusOK).JSON(&claim)
			if claim.Task == nil {
				test.Fatal("the wake enqueued no claimable run")
			}
			if claim.Task.PriorSessionID != "asking-run-session" {
				test.Fatalf("wake task prior session = %q, want asking-run-session", claim.Task.PriorSessionID)
			}
			if !strings.Contains(claim.Task.TriggerCommentContent, "Chose: Option B") {
				test.Fatalf("wake task does not carry the answer: %+v", claim.Task)
			}
		})
	}
}
