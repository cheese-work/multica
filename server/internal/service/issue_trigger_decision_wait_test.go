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
	var otherAgentID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, runtime_mode, runtime_config, runtime_id, visibility,
			max_concurrent_tasks, owner_id, instructions, custom_env, custom_args)
		SELECT workspace_id, 'other-agent', runtime_mode, runtime_config, runtime_id, visibility,
			max_concurrent_tasks, owner_id, instructions, custom_env, custom_args FROM agent WHERE id = $1
		RETURNING id`, agentID).Scan(&otherAgentID); err != nil {
		t.Fatal(err)
	}
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

	routingOn := func(agent string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE workspace SET settings = jsonb_build_object('routing_agent', jsonb_build_object('agent_id', $2::text)) WHERE id = $1`,
			workspaceID, agent); err != nil {
			t.Fatal(err)
		}
	}

	need := setIssue("need_decision")
	insertRequest("open")
	// No routing agent is configured: the step keeps its own handler, which may
	// have entry work to do. The open request must not swallow that run.
	if !enters(need) {
		t.Fatal("a configured handler's need_decision step must still start its run")
	}

	routingOn(agentID)
	if enters(need) {
		t.Fatal("the routing agent's need_decision step with an open decision request must start no run")
	}

	// The answer goes to the asking agent, not the routing agent; the routing
	// step is still a no-op.
	if _, err := pool.Exec(ctx, `UPDATE decision_request SET asking_agent_id = $2 WHERE issue_id = $1`,
		util.MustParseUUID(issueID), util.MustParseUUID(otherAgentID)); err != nil {
		t.Fatal(err)
	}
	if enters(need) {
		t.Fatal("a different asking agent leaves the routing step a no-op: no run")
	}

	if _, err := pool.Exec(ctx, `UPDATE decision_request SET status = 'answered' WHERE issue_id = $1`, util.MustParseUUID(issueID)); err != nil {
		t.Fatal(err)
	}
	if !enters(need) {
		t.Fatal("an answered request waits on nobody; the routing step's run must start")
	}
	if _, err := pool.Exec(ctx, `UPDATE decision_request SET status = 'open' WHERE issue_id = $1`, util.MustParseUUID(issueID)); err != nil {
		t.Fatal(err)
	}
	if !enters(setIssue("blocked")) {
		t.Fatal("only need_decision is covered: blocked must still start its run")
	}
}
