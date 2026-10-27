package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestAgentCannotCloseIssueWithOpenDecision(t *testing.T) {
	createTestCustomStatus(t, "agent_accepted", "done")
	for _, writer := range issueWriters {
		for _, withDescription := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/description=%t", writer.name, withDescription), func(t *testing.T) {
				fixture := newAskedRecordFixture(t)
				dbfx.Exec(t, `UPDATE issue SET status = 'in_review' WHERE id = $1`, fixture.issueID)
				body := map[string]any{"status": "agent_accepted", "title": "must not change", "suppress_run": true}
				if withDescription {
					body["description"] = "must not change"
				}
				response := writer.write(t, fixture.issueID, body, map[string]string{
					"X-Actor-Source": "task_token", "X-Agent-ID": fixture.agentID, "X-Task-ID": fixture.taskID,
				})
				if err := cancelDecisionForTest(testHandler, fixture); err != nil {
					t.Fatal(err)
				}
				stored, err := testHandler.Queries.GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{
					WorkspaceID: fixture.issue.WorkspaceID, ID: fixture.decision.ID,
				})
				if err != nil {
					t.Fatal(err)
				}
				if stored.Status != "open" || stored.CancelReason.Valid {
					t.Fatalf("close returned HTTP %d; decision status=%s cancel_reason=%s; want open without cancellation", response.Code, stored.Status, stored.CancelReason.String)
				}
				if writer.name == "batch" {
					var result struct {
						Updated int `json:"updated"`
					}
					response.Want(http.StatusOK).JSON(&result)
					if result.Updated != 0 {
						t.Fatalf("blocked batch updated %d issues", result.Updated)
					}
				} else {
					response.Want(http.StatusConflict)
					if !strings.Contains(response.Text(), uuidToString(fixture.decision.ID)) {
						t.Fatalf("conflict must identify the open request: %s", response.Text())
					}
				}
				wantIssueStatus(t, fixture.issueID, "in_review")
				var title string
				dbfx.QueryRow(t, `SELECT title FROM issue WHERE id = $1`, fixture.issueID).Scan(&title)
				if title != fixture.issue.Title {
					t.Fatalf("rejected close changed title to %q", title)
				}
			})
		}
	}
}

func TestAgentCannotCloseIssueWithUndeliveredDecisionAnswer(t *testing.T) {
	createTestCustomStatus(t, "agent_accepted", "done")
	for _, writer := range issueWriters {
		for _, withDescription := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/description=%t", writer.name, withDescription), func(t *testing.T) {
				fixture := newAskedRecordFixture(t)
				recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
				if err != nil {
					t.Fatal(err)
				}
				dbfx.Exec(t, `UPDATE issue SET status = 'in_review' WHERE id = $1`, fixture.issueID)
				body := map[string]any{"status": "agent_accepted", "suppress_run": true}
				if withDescription {
					body["description"] = "must not change"
				}
				response := writer.write(t, fixture.issueID, body, map[string]string{
					"X-Actor-Source": "task_token", "X-Agent-ID": fixture.agentID, "X-Task-ID": fixture.taskID,
				})
				if err := cancelDecisionForTest(testHandler, fixture); err != nil {
					t.Fatal(err)
				}
				request, err := testHandler.Queries.GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{
					WorkspaceID: fixture.issue.WorkspaceID, ID: fixture.decision.ID,
				})
				if err != nil {
					t.Fatal(err)
				}
				answer, err := testHandler.Queries.GetDecisionAnswer(context.Background(), db.GetDecisionAnswerParams{
					WorkspaceID: fixture.issue.WorkspaceID, ID: recorded.Answer.ID,
				})
				if err != nil {
					t.Fatal(err)
				}
				if request.Status != "answered" || request.CancelReason.Valid || answer.WakeOutcome.String == "skipped_ineligible" || answer.WokenAt.Valid || answer.DisclaimedAt.Valid {
					t.Fatalf("close returned HTTP %d; decision status=%s cancel_reason=%s; answer wake_outcome=%s woken=%t disclaimed=%t; want answered with a live undelivered answer", response.Code, request.Status, request.CancelReason.String, answer.WakeOutcome.String, answer.WokenAt.Valid, answer.DisclaimedAt.Valid)
				}
				if writer.name == "batch" {
					var result struct {
						Updated int `json:"updated"`
					}
					response.Want(http.StatusOK).JSON(&result)
					if result.Updated != 0 {
						t.Fatalf("blocked batch updated %d issues", result.Updated)
					}
				} else {
					response.Want(http.StatusConflict)
				}
				wantIssueStatus(t, fixture.issueID, "in_review")
			})
		}
	}
}

func TestOpenDecisionStatusGuardBoundaries(t *testing.T) {
	createTestCustomStatus(t, "shipped", "done")
	createTestCustomStatus(t, "approved", "done")
	cases := []struct {
		name, actor, target, requestStatus string
		wantCode                           int
	}{
		{"agent done", "agent", "done", "open", http.StatusConflict},
		{"agent custom done", "agent", "shipped", "open", http.StatusConflict},
		{"member-only approval", "agent", "approved", "open", http.StatusForbidden},
		{"review stays allowed", "agent", "in_review", "open", http.StatusOK},
		{"explicit cancellation", "agent", "cancelled", "open", http.StatusOK},
		{"answered request awaiting wake", "agent", "done", "answered", http.StatusConflict},
		{"disclaimed answer", "agent", "done", "disclaimed", http.StatusOK},
		{"withdrawn request", "agent", "done", "cancelled", http.StatusOK},
		{"human override", "member", "done", "open", http.StatusOK},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newAskedRecordFixture(t)
			if testCase.requestStatus == "answered" || testCase.requestStatus == "disclaimed" {
				recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
				if err != nil {
					t.Fatal(err)
				}
				if testCase.requestStatus == "disclaimed" {
					dbfx.Exec(t, `UPDATE decision_answer SET disclaimed_at = clock_timestamp() WHERE id = $1`, recorded.Answer.ID)
				}
			} else {
				dbfx.Exec(t, `UPDATE decision_request SET status = $2 WHERE id = $1`, fixture.decision.ID, testCase.requestStatus)
			}
			headers := map[string]string{}
			if testCase.actor == "agent" {
				headers = map[string]string{"X-Actor-Source": "task_token", "X-Agent-ID": fixture.agentID}
			}
			issueWriters[0].write(t, fixture.issueID, map[string]any{
				"status": testCase.target, "suppress_run": true,
			}, headers).Want(testCase.wantCode)
			wantStatus := testCase.target
			if testCase.wantCode != http.StatusOK {
				wantStatus = fixture.issue.Status
			}
			wantIssueStatus(t, fixture.issueID, wantStatus)
		})
	}
}

func TestOpenDecisionBlocksDefaultWorkflowHandoff(t *testing.T) {
	resetDecisionLease(t)
	createTestCustomStatus(t, "agent_accepted", "done")
	fixture := newAskedRecordFixture(t)
	leadID := createHandlerTestAgent(t, "decision workflow lead", []byte("[]"))
	projectID := createWorkflowTestProject(t, "Decision workflow project")
	dbfx.Exec(t, `UPDATE project SET lead_type = 'agent', lead_id = $1 WHERE id = $2`, leadID, projectID)
	setColumnHandoff(t, projectID, "in_review", map[string]any{
		"handler": map[string]any{"type": "agent", "id": fixture.agentID}, "next_status_key": "agent_accepted",
	}).Want(http.StatusOK)
	setColumnHandoff(t, projectID, "agent_accepted", map[string]any{
		"handler": map[string]any{"type": "project_lead"},
	}).Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE issue SET project_id = $2, status = 'in_review' WHERE id = $1`, fixture.issueID, projectID)
	tasksBefore := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, fixture.issueID)
	headers := map[string]string{"X-Actor-Source": "task_token", "X-Agent-ID": fixture.agentID, "X-Task-ID": fixture.taskID}
	body := map[string]any{"status": "agent_accepted"}
	issueWriters[0].write(t, fixture.issueID, body, headers).Want(http.StatusConflict)
	status, assigneeType, assigneeID := issueStatusAndAssignee(t, fixture.issueID)
	if status != "in_review" || assigneeType != "agent" || assigneeID != fixture.agentID {
		t.Fatalf("rejected workflow edge changed issue: %s %s %s", status, assigneeType, assigneeID)
	}
	if tasksAfter := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, fixture.issueID); tasksAfter != tasksBefore {
		t.Fatalf("rejected workflow edge dispatched a task: before=%d after=%d", tasksBefore, tasksAfter)
	}
	if err := cancelDecisionForTest(testHandler, fixture); err != nil {
		t.Fatal(err)
	}
	var requestStatus string
	dbfx.QueryRow(t, `SELECT status FROM decision_request WHERE id = $1`, fixture.decision.ID).Scan(&requestStatus)
	if requestStatus != "open" {
		t.Fatalf("rejected workflow edge cancelled the request: %s", requestStatus)
	}
	recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		t.Fatal(err)
	}
	issueWriters[0].write(t, fixture.issueID, body, headers).Want(http.StatusConflict)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	dbfx.Exec(t, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE id = $1`, recorded.Answer.ID)
	dbfx.Exec(t, `UPDATE decision_intent SET sealed_at = clock_timestamp() WHERE id = $1`, fixture.intentID)
	_, runner := newDecisionRunnerFixture(t)
	decisionPass(t, runner)
	answer, err := testHandler.Queries.GetDecisionAnswer(context.Background(), db.GetDecisionAnswerParams{
		WorkspaceID: fixture.issue.WorkspaceID, ID: recorded.Answer.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !answer.WokenAt.Valid || answer.WakeOutcome.String != "run_enqueued" {
		t.Fatalf("answer not delivered before close: woken=%t outcome=%s", answer.WokenAt.Valid, answer.WakeOutcome.String)
	}
	issueWriters[0].write(t, fixture.issueID, body, headers).Want(http.StatusOK)
	status, assigneeType, assigneeID = issueStatusAndAssignee(t, fixture.issueID)
	if status != "agent_accepted" || assigneeType != "agent" || assigneeID != leadID {
		t.Fatalf("delivered decision did not release workflow edge: %s %s %s", status, assigneeType, assigneeID)
	}
}

func TestAgentCloseSerializesWithDecisionRequest(t *testing.T) {
	for _, writer := range issueWriters {
		for _, withDescription := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/description=%t", writer.name, withDescription), func(t *testing.T) {
				fixture := newAskedRecordFixture(t)
				dbfx.Exec(t, `UPDATE decision_request SET status = 'cancelled' WHERE id = $1`, fixture.decision.ID)
				body := map[string]any{"status": "done", "suppress_run": true}
				if withDescription {
					body["description"] = "must not change"
				}
				response := raceAgainst(t, func(tx pgx.Tx) {
					if _, err := db.New(tx).LockDecisionIssue(context.Background(), db.LockDecisionIssueParams{
						WorkspaceID: fixture.issue.WorkspaceID, ID: fixture.issue.ID,
					}); err != nil {
						t.Fatal(err)
					}
					if _, err := tx.Exec(context.Background(), `UPDATE decision_request SET status = 'open' WHERE id = $1`, fixture.decision.ID); err != nil {
						t.Fatal(err)
					}
				}, func() *testutil.Response {
					return writer.write(t, fixture.issueID, body, map[string]string{
						"X-Actor-Source": "task_token", "X-Agent-ID": fixture.agentID,
					})
				})
				if writer.name == "batch" {
					response.Want(http.StatusOK)
				} else {
					response.Want(http.StatusConflict)
				}
				wantIssueStatus(t, fixture.issueID, fixture.issue.Status)
			})
		}
	}
}
