package service

import (
	"context"
	"encoding/json"
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

	// stepOn stores a project workflow whose need_decision step is handled by
	// agent, with or without the explicit waits_on_open_decision declaration, and
	// points the issue at it.
	stepOn := func(agent string, waits bool) {
		t.Helper()
		step := map[string]any{
			"status_key":   "need_decision",
			"handler":      map[string]any{"type": "agent", "id": agent},
			"instructions": "Entry work: file the decision brief before waiting.",
		}
		if waits {
			step["waits_on_open_decision"] = true
		}
		raw, _ := json.Marshal([]map[string]any{{"status_key": "in_progress"}, step})
		var workflowID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO issue_workflow (workspace_id, name, initial_status_key, steps)
			VALUES ($1, 'decision-wait-'||gen_random_uuid()::text, 'in_progress', $2::jsonb) RETURNING id::text`,
			workspaceID, string(raw)).Scan(&workflowID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE issue SET workflow_id = $2 WHERE id = $1`,
			util.MustParseUUID(issueID), workflowID); err != nil {
			t.Fatal(err)
		}
	}

	insertRequest("open")
	// The step is not declared a no-op: it may have entry work, even for the
	// workspace routing agent. The open request must not swallow that run.
	stepOn(agentID, false)
	need := setIssue("need_decision")
	if !enters(need) {
		t.Fatal("a need_decision step that does not declare waits_on_open_decision must still start its run")
	}

	stepOn(agentID, true)
	need = setIssue("need_decision")
	if enters(need) {
		t.Fatal("a declared waits_on_open_decision step with an open decision request must start no run")
	}

	// The answer goes to the asking agent, not the step's handler; the declared
	// step is still a no-op.
	if _, err := pool.Exec(ctx, `UPDATE decision_request SET asking_agent_id = $2 WHERE issue_id = $1`,
		util.MustParseUUID(issueID), util.MustParseUUID(otherAgentID)); err != nil {
		t.Fatal(err)
	}
	if enters(need) {
		t.Fatal("a different asking agent leaves the declared step a no-op: no run")
	}

	if _, err := pool.Exec(ctx, `UPDATE decision_request SET status = 'answered' WHERE issue_id = $1`, util.MustParseUUID(issueID)); err != nil {
		t.Fatal(err)
	}
	if !enters(need) {
		t.Fatal("an answered request waits on nobody; the step's run must start")
	}
	if _, err := pool.Exec(ctx, `UPDATE decision_request SET status = 'open' WHERE issue_id = $1`, util.MustParseUUID(issueID)); err != nil {
		t.Fatal(err)
	}
	if !enters(setIssue("blocked")) {
		t.Fatal("only need_decision is covered: blocked must still start its run")
	}
}
