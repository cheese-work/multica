package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-4976: an issue that enters need_decision while a decision request is open
// waits for the answer, and the answer wakes it. The routing run the step would
// start has nothing to route, so the handoff starts no run.
func TestWillEnqueueRunSkipsNeedDecisionStepWithOpenDecisionRequest(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	queries := db.New(pool)
	svc := &IssueService{Queries: queries, TaskService: &TaskService{Queries: queries}}

	setIssue := func(status string) db.Issue {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE issue SET status = $2, assignee_type = 'agent', assignee_id = $3 WHERE id = $1`,
			util.MustParseUUID(issueID), status, util.MustParseUUID(agentID)); err != nil {
			t.Fatal(err)
		}
		issue, err := svc.Queries.GetIssue(ctx, util.MustParseUUID(issueID))
		if err != nil {
			t.Fatal(err)
		}
		return issue
	}
	enters := func(issue db.Issue) bool {
		_, ok := svc.WillEnqueueRun(ctx, IssueTriggerInput{Issue: issue, PrevStatus: "in_progress", AssigneeChanged: true, StatusChanged: true, EnteredHandoffStep: true}, IssueTriggerProbe{})
		return ok
	}
	insertRequest := func(status string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO decision_request (workspace_id, issue_id, asking_agent_id, human_user_id, chat_session_id, escalation_class, origin, status)
			VALUES ($1, $2, $3, $4, gen_random_uuid(), 'product_direction', 'asked', $5)`,
			workspaceID, issueID, agentID, userID, status); err != nil {
			t.Fatal(err)
		}
	}

	need := setIssue("need_decision")
	if !enters(need) {
		t.Fatal("need_decision with no decision request must still start the step's run")
	}
	insertRequest("answered")
	if !enters(need) {
		t.Fatal("an answered request waits on nobody; the step's run must start")
	}
	insertRequest("open")
	if enters(need) {
		t.Fatal("need_decision with an open decision request must start no run")
	}
	if !enters(setIssue("blocked")) {
		t.Fatal("only need_decision is covered: blocked must still start its run")
	}
}
