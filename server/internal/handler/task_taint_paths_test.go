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

// A manual rerun adopts the exact source's session. A later task of the same
// session can have taken taint the source never saw; the rerun inherits it.
func TestRerunOfCleanTaskInTaintedSessionClaimsRestricted(t *testing.T) {
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Taint rerun runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Taint rerun agent")
	insertGrant(t, issueID, testWorkspaceID, []string{"docker"}, "1 minute", "1 hour", false)
	clean := dbfx.Task(t, agentID, testutil.Cols{
		"status": "completed", "runtime_id": runtimeID, "issue_id": issueID,
		"session_id": "sess-shared", "started_at": testutil.Raw("now() - interval '2 minutes'"), "completed_at": testutil.Raw("now() - interval '90 seconds'"),
	})
	dbfx.Task(t, agentID, testutil.Cols{
		"status": "completed", "runtime_id": runtimeID, "issue_id": issueID,
		"session_id": "sess-shared", "started_at": testutil.Raw("now() - interval '1 minute'"), "completed_at": testutil.Raw("now()"),
		"taint_ids": []string{"t1"},
	})
	dbfx.Exec(t, `INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, rerun_of_task_id, force_fresh_session)
		VALUES ($1, $2, $3, 'queued', 0, $4, TRUE)`, agentID, runtimeID, issueID, clean)

	claimRestrictedWithTaint(t, runtimeID, []string{"t1"})
}

// The assignment run already queued for the target is the one that survives
// de-duplication, so it must take the assigner's taint.
func TestAgentAssignmentTaintsSurvivingQueuedRun(t *testing.T) {
	chain, targetID, runtimeID := taintedAssigner(t)
	insertGrant(t, chain.IssueY, testWorkspaceID, []string{"docker"}, "1 minute", "1 hour", false)
	dbfx.Task(t, targetID, testutil.Cols{"status": "queued", "runtime_id": runtimeID, "issue_id": chain.IssueY})

	agentAssigns(t, chain.CoordinatorID, chain.TaskA, chain.IssueY, targetID).Want(http.StatusOK)

	var got []string
	dbfx.QueryRow(t, `SELECT taint_ids FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, chain.IssueY, targetID).Scan(&got)
	if !reflect.DeepEqual(got, []string{"t1"}) {
		t.Fatalf("surviving queued task taint = %v, want [t1]", got)
	}
	claimRestrictedWithTaint(t, runtimeID, []string{"t1"})
}

// An acting task that cannot be read must not become an untainted dispatch.
func TestAgentAssignmentFailsClosedWhenActingTaskUnreadable(t *testing.T) {
	chain, targetID, _ := taintedAssigner(t)

	agentAssigns(t, chain.CoordinatorID, "00000000-0000-4000-8000-000000000001", chain.IssueY, targetID)

	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, chain.IssueY, targetID).Scan(&n)
	if n != 0 {
		t.Fatalf("queued tasks for the target = %d, want 0 when the acting task cannot be read", n)
	}
}

// A run already dispatched when a tainted agent assigns the issue still reads
// the issue afterwards, so it takes the assigner's taint before the write.
func TestAgentAssignmentTaintsRunDispatchedBeforeTheWrite(t *testing.T) {
	chain, targetID, runtimeID := taintedAssigner(t)
	dbfx.Task(t, targetID, testutil.Cols{"status": "dispatched", "runtime_id": runtimeID, "issue_id": chain.IssueY, "dispatched_at": testutil.Raw("now()")})

	agentAssigns(t, chain.CoordinatorID, chain.TaskA, chain.IssueY, targetID).Want(http.StatusOK)

	var got []string
	dbfx.QueryRow(t, `SELECT taint_ids FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'dispatched'`, chain.IssueY, targetID).Scan(&got)
	if !reflect.DeepEqual(got, []string{"t1"}) {
		t.Fatalf("dispatched task taint = %v, want [t1]", got)
	}
}

// Taint that lands on a task after the claim dispatched it, and before the
// response is built, is in the response: the claim reads the issue first and
// the taint last. A trigger stands in for the concurrent writer, which cannot
// be paused inside the handler.
func TestClaimResponseCarriesTaintWrittenAfterDispatch(t *testing.T) {
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Taint late runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Taint late agent")
	insertGrant(t, issueID, testWorkspaceID, []string{"docker"}, "1 minute", "1 hour", false)
	dbfx.Exec(t, `CREATE FUNCTION test_late_taint() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN UPDATE agent_task_queue SET taint_ids = ARRAY['t1'] WHERE id = NEW.id; RETURN NULL; END $$`)
	dbfx.Exec(t, `CREATE TRIGGER test_late_taint AFTER UPDATE ON agent_task_queue FOR EACH ROW
		WHEN (NEW.status = 'dispatched' AND OLD.status = 'queued' AND NEW.agent_id = '`+agentID+`') EXECUTE FUNCTION test_late_taint()`)
	t.Cleanup(func() {
		dbfx.Exec(t, `DROP TRIGGER IF EXISTS test_late_taint ON agent_task_queue`)
		dbfx.Exec(t, `DROP FUNCTION IF EXISTS test_late_taint()`)
	})
	dbfx.Task(t, agentID, testutil.Cols{"status": "queued", "runtime_id": runtimeID, "issue_id": issueID})

	claimRestrictedWithTaint(t, runtimeID, []string{"t1"})
}
