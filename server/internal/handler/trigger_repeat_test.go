package handler

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// claimTriggerRepeat claims the task triggered by triggerID and returns the
// repeat signal the claim carried.
func claimTriggerRepeat(t *testing.T, ctx context.Context, issueID, agentID, runtimeID, triggerID string) *AgentTaskResponse {
	t.Helper()
	dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":         runtimeID,
		"issue_id":           issueID,
		"trigger_comment_id": triggerID,
	})
	req := newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil)
	req = withURLParam(req, "runtimeId", runtimeID)
	var resp struct {
		Task *AgentTaskResponse `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK).JSON(&resp)
	if resp.Task == nil {
		t.Fatal("expected a claimed task")
	}
	return resp.Task
}

// CHE-9042 (retro R10): the third identical Cheese message carries count 2 and
// the two earlier ids, once, not once per copy. Whitespace-only differences
// still count as a repeat; a deleted copy does not.
func TestClaimTaskByRuntime_TriggerRepeatSignal(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Repeat runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Repeat agent")
	at := func(offset string) testutil.Cols {
		return testutil.Cols{"created_at": testutil.Raw("now() - interval '" + offset + "'")}
	}
	first := dbfx.Comment(t, issueID, "please fix the routing", at("5 minutes"))
	deleted := dbfx.Comment(t, issueID, "please fix the routing", testutil.Cols{
		"created_at": testutil.Raw("now() - interval '4 minutes'"),
		"deleted_at": testutil.Raw("now()"),
	})
	_ = deleted
	other := dbfx.Comment(t, issueID, "something else entirely", at("3 minutes"))
	_ = other
	second := dbfx.Comment(t, issueID, "  please  fix\nthe routing ", at("2 minutes"))
	third := dbfx.Comment(t, issueID, "please fix the routing", at("1 minute"))

	task := claimTriggerRepeat(t, ctx, issueID, agentID, runtimeID, third)
	if task.TriggerRepeatCount != 2 {
		t.Fatalf("TriggerRepeatCount = %d, want 2", task.TriggerRepeatCount)
	}
	if want := []string{first, second}; !reflect.DeepEqual(task.TriggerRepeatIDs, want) {
		t.Fatalf("TriggerRepeatIDs = %v, want %v (oldest first, deleted copy excluded)", task.TriggerRepeatIDs, want)
	}
}

// A first-time message, and an agent's repeat of its own words, carry no signal.
func TestClaimTaskByRuntime_TriggerRepeatSignalAbsentWhenNotARepeat(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "No repeat runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "No repeat agent")
	dbfx.Comment(t, issueID, "unique message", testutil.Cols{"created_at": testutil.Raw("now() - interval '3 minutes'")})
	firstTime := dbfx.Comment(t, issueID, "another unique message", testutil.Cols{"created_at": testutil.Raw("now() - interval '2 minutes'")})

	task := claimTriggerRepeat(t, ctx, issueID, agentID, runtimeID, firstTime)
	if task.TriggerRepeatCount != 0 || len(task.TriggerRepeatIDs) != 0 {
		t.Fatalf("first-time message carried repeat signal: %d %v", task.TriggerRepeatCount, task.TriggerRepeatIDs)
	}

}

// Agent authors are out of scope even when they repeat themselves.
func TestClaimTaskByRuntime_TriggerRepeatSignalIgnoresAgentAuthors(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Agent repeat runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Agent repeat agent")
	dbfx.Comment(t, issueID, "agent says the same", testutil.Cols{"author_type": "agent", "author_id": agentID, "created_at": testutil.Raw("now() - interval '90 seconds'")})
	agentRepeat := dbfx.Comment(t, issueID, "agent says the same", testutil.Cols{"author_type": "agent", "author_id": agentID})
	task := claimTriggerRepeat(t, ctx, issueID, agentID, runtimeID, agentRepeat)
	if task.TriggerRepeatCount != 0 {
		t.Fatalf("agent-authored repeat carried signal: %d", task.TriggerRepeatCount)
	}
}
