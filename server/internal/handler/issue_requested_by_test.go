package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// requestedByTestUser adds a workspace member who is not the fixture owner.
func requestedByTestUser(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var userID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO "user" (name, email) VALUES ('Requester', $1) RETURNING id::text`,
		"requester-"+workflowTestSuffix()+"@example.test").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`, testWorkspaceID, userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID) })
	return userID
}

// formerMemberTestUser is a user whose member row is gone: an id that can
// still sit in issue.requested_by_id or project.lead_id after removal.
func formerMemberTestUser(t *testing.T) string {
	t.Helper()
	userID := requestedByTestUser(t)
	if _, err := testPool.Exec(context.Background(),
		`DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, userID); err != nil {
		t.Fatal(err)
	}
	return userID
}

// agentCreateIssue posts a create the way a task-token agent run does.
func agentCreateIssue(t *testing.T, agentID, taskID string, body map[string]any) *testutil.Response {
	t.Helper()
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", agentID)
	req.Header.Set("X-Task-ID", taskID)
	return testutil.Call(t, testHandler.CreateIssue, req)
}

func issueRequestedBy(t *testing.T, issueID string) string {
	t.Helper()
	var id *string
	dbfx.QueryRow(t, `SELECT requested_by_id::text FROM issue WHERE id = $1`, issueID).Scan(&id)
	if id == nil {
		return ""
	}
	return *id
}

// An agent that creates an issue for a person records that person, and the
// issue API returns it.
func TestAgentCreateStoresTheMemberItActsFor(t *testing.T) {
	seedTestCatalog(t)
	syncIssueCounter(t)
	agentID := createHandlerTestAgent(t, "requested-by-agent", nil)
	taskID := insertHandlerTestTask(t, agentID)
	requester := requestedByTestUser(t)

	var created IssueResponse
	agentCreateIssue(t, agentID, taskID, map[string]any{
		"title": "on behalf " + workflowTestSuffix(), "requested_by_id": requester,
	}).Want(http.StatusCreated).JSON(&created)
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, created.ID) })

	if created.RequestedByID == nil || *created.RequestedByID != requester {
		t.Fatalf("create response requested_by_id = %v, want %s", created.RequestedByID, requester)
	}
	if got := issueRequestedBy(t, created.ID); got != requester {
		t.Fatalf("stored requested_by_id = %q, want %s", got, requester)
	}
	var fetched IssueResponse
	testutil.Call(t, testHandler.GetIssue, withURLParam(newRequest("GET", "/api/issues/"+created.ID, nil), "id", created.ID)).
		Want(http.StatusOK).JSON(&fetched)
	if fetched.RequestedByID == nil || *fetched.RequestedByID != requester {
		t.Fatalf("GET requested_by_id = %v, want %s", fetched.RequestedByID, requester)
	}
}

// With no explicit member, an agent run acts for the human at the top of its
// chain; a person who is not a workspace member is refused.
func TestAgentCreateDefaultsToTheRunsOriginator(t *testing.T) {
	seedTestCatalog(t)
	syncIssueCounter(t)
	agentID := createHandlerTestAgent(t, "requested-by-originator", nil)
	taskID := insertHandlerTestTask(t, agentID)
	requester := requestedByTestUser(t)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE agent_task_queue SET originator_source = 'direct_human', originator_user_id = $2, accountable_user_id = $2 WHERE id = $1`, taskID, requester); err != nil {
		t.Fatal(err)
	}

	var created IssueResponse
	agentCreateIssue(t, agentID, taskID, map[string]any{"title": "originator " + workflowTestSuffix()}).
		Want(http.StatusCreated).JSON(&created)
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, created.ID) })
	if got := issueRequestedBy(t, created.ID); got != requester {
		t.Fatalf("stored requested_by_id = %q, want the originator %s", got, requester)
	}

	agentCreateIssue(t, agentID, taskID, map[string]any{
		"title": "stranger " + workflowTestSuffix(), "requested_by_id": "00000000-0000-4000-8000-000000000001",
	}).Want(http.StatusBadRequest)
}

// A blocked issue in a workflow whose blocked step hands to a human by role
// lands on the requester, then the project lead, then the workspace owner.
func TestBlockedStepHandsToAHumanByRole(t *testing.T) {
	seedTestCatalog(t)
	syncIssueCounter(t)
	requester := requestedByTestUser(t)
	lead := requestedByTestUser(t)
	former := formerMemberTestUser(t)
	wf := createTestWorkflow(t, "Human by role "+workflowTestSuffix(), "in_progress")
	setWorkflowHandoff(t, wf.ID, "blocked", map[string]any{
		"handler": map[string]any{"type": "human_by_role"}, "instructions": "A person decides.",
	}).Want(http.StatusOK)
	projectID := createWorkflowTestProject(t, "Human by role project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)

	cases := []struct {
		name        string
		requestedBy string
		lead        string
		want        string
	}{
		{"requester first", requester, lead, requester},
		{"no requester: project lead", "", lead, lead},
		{"no lead: workspace owner", "", "", testUserID},
		{"former-member requester: project lead", former, lead, lead},
		{"former-member requester and lead: workspace owner", former, former, testUserID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var rb, leadType, leadID any
			if tc.requestedBy != "" {
				rb = tc.requestedBy
			}
			if tc.lead != "" {
				leadType, leadID = "member", tc.lead
			}
			if _, err := testPool.Exec(ctx, `UPDATE project SET lead_type = $2, lead_id = $3 WHERE id = $1`, projectID, leadType, leadID); err != nil {
				t.Fatal(err)
			}
			issueID := dbfx.Issue(t, "blocked by role "+workflowTestSuffix(), testutil.Cols{"project_id": projectID, "status": "in_progress"})
			if _, err := testPool.Exec(ctx, `UPDATE issue SET requested_by_id = $2 WHERE id = $1`, issueID, rb); err != nil {
				t.Fatal(err)
			}

			updateIssueForTest(t, issueID, map[string]any{"status": "blocked"}).Want(http.StatusOK)
			status, assigneeType, assigneeID := issueStatusAndAssignee(t, issueID)
			if status != "blocked" || assigneeType != "member" || assigneeID != tc.want {
				t.Fatalf("issue = %s %s %s, want blocked member %s", status, assigneeType, assigneeID, tc.want)
			}
		})
	}
}

// A failed lookup is not "no such person": the write must fail, not continue
// without a requester or a human assignee.
func TestHumanLookupFailuresAreNotTreatedAsMissingPeople(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	member := requestedByTestUser(t)

	t.Run("explicit requester", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := newRequest("POST", "/api/issues", nil).WithContext(canceled)
		explicit := member
		if _, ok := testHandler.resolveRequestedBy(w, r, testWorkspaceID, "member", testUserID, &explicit); ok || w.Code != http.StatusInternalServerError {
			t.Fatalf("ok = %v, status = %d, want a refused create with 500", ok, w.Code)
		}
	})
	t.Run("default requester", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := newRequest("POST", "/api/issues", nil).WithContext(canceled)
		if _, ok := testHandler.resolveRequestedBy(w, r, testWorkspaceID, "member", member, nil); ok || w.Code != http.StatusInternalServerError {
			t.Fatalf("ok = %v, status = %d, want a refused create with 500", ok, w.Code)
		}
	})
	t.Run("requester membership", func(t *testing.T) {
		step := issueworkflow.Step{Handler: issueworkflow.Handler{Type: issueworkflow.HandlerHumanByRole}}
		if _, err := testHandler.workflowHumans(canceled, step, db.Project{}, parseUUID(testWorkspaceID), parseUUID(member)); err == nil {
			t.Fatal("workflowHumans returned no error for a failed membership lookup")
		}
	})
	t.Run("member project lead membership", func(t *testing.T) {
		step := issueworkflow.Step{Handler: issueworkflow.Handler{Type: issueworkflow.HandlerHumanByRole}}
		project := db.Project{LeadType: pgtype.Text{String: "member", Valid: true}, LeadID: parseUUID(member)}
		if _, err := testHandler.workflowHumans(canceled, step, project, parseUUID(testWorkspaceID), pgtype.UUID{}); err == nil {
			t.Fatal("workflowHumans returned no error for a failed membership lookup")
		}
	})
	t.Run("workspace owner", func(t *testing.T) {
		step := issueworkflow.Step{Handler: issueworkflow.Handler{Type: issueworkflow.HandlerHumanByRole}}
		if _, err := testHandler.workflowHumans(canceled, step, db.Project{}, parseUUID(testWorkspaceID), pgtype.UUID{}); err == nil {
			t.Fatal("workflowHumans returned no error for a failed owner lookup")
		}
	})
}
