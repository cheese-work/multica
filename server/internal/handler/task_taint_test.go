package handler

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Taint fields and propagation (CHE-2216 plan 3.7, slice B5a; T9). The
// database trigger is the single choke point: every task insert path inherits
// the union of its sources, so these tests insert rows the way each path does.

func taintOf(t *testing.T, table, id string) []string {
	t.Helper()
	var ids []string
	dbfx.QueryRow(t, `SELECT taint_ids FROM `+table+` WHERE id = $1`, id).Scan(&ids)
	return ids
}

func taintedTask(t *testing.T, agentID string, taint ...string) string {
	t.Helper()
	return dbfx.Task(t, agentID, testutil.Cols{"status": "running", "runtime_id": handlerTestRuntimeID(t), "started_at": testutil.Raw("now()"), "taint_ids": taint})
}

func TestTaintPropagatesToEveryTaskSource(t *testing.T) {
	agentID := seededReadyAgentID(t)
	issueID := dbfx.Issue(t, "taint propagation")
	tainted := taintedTask(t, agentID, "t1")
	clean := taintedTask(t, agentID)
	// A comment written by the tainted task stores its taint IDs.
	byTainted := dbfx.Comment(t, issueID, "@agent please continue", testutil.Cols{"source_task_id": tainted})
	byClean := dbfx.Comment(t, issueID, "clean", testutil.Cols{"source_task_id": clean})
	byMember := dbfx.Comment(t, issueID, "member")

	for table, id := range map[string]string{"comment": byTainted} {
		if got := taintOf(t, table, id); !reflect.DeepEqual(got, []string{"t1"}) {
			t.Fatalf("%s %s: taint = %v, want [t1]", table, id, got)
		}
	}
	for _, id := range []string{byClean, byMember} {
		if got := taintOf(t, "comment", id); len(got) != 0 {
			t.Fatalf("comment %s: taint = %v, want none", id, got)
		}
	}

	cases := []struct {
		name string
		cols testutil.Cols
		want []string
	}{
		{"continuation (parent task)", testutil.Cols{"parent_task_id": tainted}, []string{"t1"}},
		{"retry", testutil.Cols{"retry_of_task_id": tainted}, []string{"t1"}},
		{"rerun", testutil.Cols{"rerun_of_task_id": tainted}, []string{"t1"}},
		{"trigger comment", testutil.Cols{"trigger_comment_id": byTainted}, []string{"t1"}},
		{"coalesced comment", testutil.Cols{"coalesced_comment_ids": testutil.Raw("ARRAY['" + byTainted + "']::uuid[]")}, []string{"t1"}},
		{"assigning task", testutil.Cols{"delegated_from_task_id": tainted}, []string{"t1"}},
		{"clean sources stay clean", testutil.Cols{"parent_task_id": clean, "trigger_comment_id": byClean}, []string{}},
		{"member comment stays clean", testutil.Cols{"trigger_comment_id": byMember}, []string{}},
		{"union of sources, sorted", testutil.Cols{"parent_task_id": tainted, "delegated_from_task_id": taintedTask(t, agentID, "a0", "t1")}, []string{"a0", "t1"}},
	}
	for _, c := range cases {
		cols := testutil.Cols{"status": "completed", "issue_id": issueID, "runtime_id": handlerTestRuntimeID(t)}
		for k, v := range c.cols {
			cols[k] = v
		}
		id := dbfx.Task(t, agentID, cols)
		if got := taintOf(t, "agent_task_queue", id); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: taint = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTaintOnlyGrows(t *testing.T) {
	agentID := seededReadyAgentID(t)
	issueID := dbfx.Issue(t, "taint grow-only")
	tainted := taintedTask(t, agentID, "t1")
	byTainted := dbfx.Comment(t, issueID, "tainted", testutil.Cols{"source_task_id": tainted})
	queued := dbfx.Task(t, agentID, testutil.Cols{"issue_id": issueID, "runtime_id": handlerTestRuntimeID(t)})
	if got := taintOf(t, "agent_task_queue", queued); len(got) != 0 {
		t.Fatalf("fresh queued task taint = %v", got)
	}

	// A queued task that later merges a tainted trigger comment becomes tainted.
	dbfx.Exec(t, `UPDATE agent_task_queue SET trigger_comment_id = $2 WHERE id = $1`, queued, byTainted)
	if got := taintOf(t, "agent_task_queue", queued); !reflect.DeepEqual(got, []string{"t1"}) {
		t.Fatalf("task after merging tainted comment = %v, want [t1]", got)
	}
	// Neither a task nor a comment can drop a taint ID.
	dbfx.Exec(t, `UPDATE agent_task_queue SET taint_ids = '{}' WHERE id = $1`, queued)
	dbfx.Exec(t, `UPDATE comment SET taint_ids = '{}' WHERE id = $1`, byTainted)
	if got := taintOf(t, "agent_task_queue", queued); !reflect.DeepEqual(got, []string{"t1"}) {
		t.Errorf("task taint cleared by update: %v", got)
	}
	if got := taintOf(t, "comment", byTainted); !reflect.DeepEqual(got, []string{"t1"}) {
		t.Errorf("comment taint cleared by update: %v", got)
	}
}

func TestClaimOfTaintedTaskIgnoresActiveGrant(t *testing.T) {
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Taint claim runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Taint claim agent")
	insertGrant(t, issueID, testWorkspaceID, []string{"docker"}, "1 minute", "1 hour", false)
	taskID := createDispatchedClaimFixtureTask(t, ctx, agentID, runtimeID, issueID, "120 seconds", false)
	// The assigning task was tainted: the claimed task inherits it.
	dbfx.Exec(t, `UPDATE agent_task_queue SET delegated_from_task_id = $2 WHERE id = $1`, taskID, taintedTask(t, agentID, "t1"))

	req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil), "runtimeId", runtimeID)
	var resp struct {
		Task *struct {
			Profile  string   `json:"profile"`
			Scopes   []string `json:"scopes"`
			TaintIDs []string `json:"taint_ids"`
		} `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK).JSON(&resp)
	if resp.Task == nil || resp.Task.Profile != "restricted" || len(resp.Task.Scopes) != 0 || !reflect.DeepEqual(resp.Task.TaintIDs, []string{"t1"}) {
		t.Fatalf("claim = %+v, want restricted with taint [t1] despite the grant", resp.Task)
	}
}
