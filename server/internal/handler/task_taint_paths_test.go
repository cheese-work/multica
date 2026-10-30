package handler

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// T9 through the real paths (CHE-2216 plan 3.7): a tainted task is continued by
// a normal resume, or assigns an issue, and the successor claims restricted
// even with an active grant. Nothing here sets a lineage column by hand.

func claimRestrictedWithTaint(t *testing.T, runtimeID string, want []string) {
	t.Helper()
	req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil), "runtimeId", runtimeID)
	var resp struct {
		Task *struct {
			Profile  string   `json:"profile"`
			Scopes   []string `json:"scopes"`
			TaintIDs []string `json:"taint_ids"`
		} `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK).JSON(&resp)
	if resp.Task == nil || resp.Task.Profile != "restricted" || len(resp.Task.Scopes) != 0 || !reflect.DeepEqual(resp.Task.TaintIDs, want) {
		t.Fatalf("claim = %+v, want restricted with taint %v despite the grant", resp.Task, want)
	}
}

func TestNormalResumeOfTaintedSessionClaimsRestricted(t *testing.T) {
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Taint resume runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Taint resume agent")
	insertGrant(t, issueID, testWorkspaceID, []string{"docker"}, "1 minute", "1 hour", false)
	// The tainted predecessor finished a session on this issue. The successor is
	// enqueued the normal way: no session, parent or delegation column.
	dbfx.Task(t, agentID, testutil.Cols{
		"status": "completed", "runtime_id": runtimeID, "issue_id": issueID,
		"session_id": "sess-tainted", "started_at": testutil.Raw("now() - interval '1 minute'"), "completed_at": testutil.Raw("now()"),
		"taint_ids": []string{"t1"},
	})
	createDispatchedClaimFixtureTask(t, ctx, agentID, runtimeID, issueID, "120 seconds", false)

	claimRestrictedWithTaint(t, runtimeID, []string{"t1"})
}

// taintedAssigner is a chain whose running task is tainted, and a target agent
// the member may invoke on a runtime of its own.
func taintedAssigner(t *testing.T) (chain crossIssueChain, targetID, runtimeID string) {
	t.Helper()
	ctx := context.Background()
	runtimeID = createClaimReclaimRuntime(t, ctx, "Taint assign runtime")
	targetID, _ = createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Taint assign target")
	chain = newCrossIssueChain(t, testUserID, testUserID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET taint_ids = ARRAY['t1'] WHERE id = $1`, chain.TaskA)
	dbfx.Exec(t, `UPDATE agent SET permission_mode = 'public_to', owner_id = $2 WHERE id = $1`, targetID, testUserID)
	dbfx.InsertNoID(t, "agent_invocation_target", testutil.Cols{"agent_id": targetID, "target_type": "member", "target_id": testUserID},
		"agent_id = $1 AND target_type = 'member' AND target_id = $2", targetID, testUserID)
	return chain, targetID, runtimeID
}

func TestAgentAssignmentCarriesAssigningTaskTaint(t *testing.T) {
	chain, targetID, runtimeID := taintedAssigner(t)
	insertGrant(t, chain.IssueY, testWorkspaceID, []string{"docker"}, "1 minute", "1 hour", false)

	agentAssigns(t, chain.CoordinatorID, chain.TaskA, chain.IssueY, targetID).Want(http.StatusOK)

	var got []string
	dbfx.QueryRow(t, `SELECT taint_ids FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, chain.IssueY, targetID).Scan(&got)
	if !reflect.DeepEqual(got, []string{"t1"}) {
		t.Fatalf("assigned task taint = %v, want [t1]", got)
	}
	claimRestrictedWithTaint(t, runtimeID, []string{"t1"})
}

// Coverage, not a fix: creation already records the creating task as the
// delegator, which the task trigger reads. Kept so that stays true.
func TestAgentCreatedIssueCarriesCreatingTaskTaint(t *testing.T) {
	chain, targetID, _ := taintedAssigner(t)

	var created struct {
		ID string `json:"id"`
	}
	agentCreatesSubIssue(t, chain.CoordinatorID, chain.TaskA, chain.IssueX, targetID).Want(http.StatusCreated).JSON(&created)

	var got []string
	dbfx.QueryRow(t, `SELECT taint_ids FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, created.ID, targetID).Scan(&got)
	if !reflect.DeepEqual(got, []string{"t1"}) {
		t.Fatalf("task for the created issue: taint = %v, want [t1]", got)
	}
}
