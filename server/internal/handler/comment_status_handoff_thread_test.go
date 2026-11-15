package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6120 (S1 of CHE-6110): a run started by a status handoff posts its plain
// comment into the thread of its handoff comment instead of being refused (409)
// or opening a thread of its own.

type handoffThreadFixture struct {
	IssueID    string
	AgentID    string
	TaskID     string
	TriggerID  string
	AssigneeID string
}

// newHandoffThreadFixture builds a running task for a fresh agent on an issue
// assigned to another agent A. triggerKind sets agent_task_queue.trigger_kind;
// makeTrigger builds the task's trigger comment (authored by A).
func newHandoffThreadFixture(t *testing.T, triggerKind string, makeTrigger func(issueID, authorAgentID string) string) handoffThreadFixture {
	t.Helper()
	agentA := createHandlerTestAgent(t, "che6120-agent-a", nil)
	agentB := createHandlerTestAgent(t, "che6120-agent-b", nil)
	issueID := dbfx.Issue(t, "che6120 thread", testutil.Cols{
		"assignee_type": "agent",
		"assignee_id":   agentA,
	})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})
	trigger := makeTrigger(issueID, agentA)
	taskID := dbfx.Task(t, agentB, testutil.Cols{
		"runtime_id":         handlerTestRuntimeID(t),
		"issue_id":           issueID,
		"trigger_comment_id": trigger,
		"trigger_kind":       triggerKind,
		"status":             "running",
		"started_at":         testutil.Raw("now()"),
	})
	return handoffThreadFixture{IssueID: issueID, AgentID: agentB, TaskID: taskID, TriggerID: trigger, AssigneeID: agentA}
}

func (fx handoffThreadFixture) post(t *testing.T, body map[string]any) (int, map[string]any) {
	t.Helper()
	req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+fx.IssueID+"/comments", body), "id", fx.IssueID)
	req.Header.Set("X-Agent-ID", fx.AgentID)
	req.Header.Set("X-Task-ID", fx.TaskID)
	rec := testutil.Call(t, testHandler.CreateComment, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func handoffParentOf(out map[string]any) string {
	if p, ok := out["parent_id"].(string); ok {
		return p
	}
	return ""
}

func TestCreateComment_StatusHandoffTopLevelTriggerJoinsIt(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newHandoffThreadFixture(t, "status_handoff", func(issueID, a string) string {
		return dbfx.Comment(t, issueID, "handoff note from A", testutil.Cols{"author_type": "agent", "author_id": a})
	})
	code, out := fx.post(t, map[string]any{"content": "result of B"})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, out)
	}
	if got := handoffParentOf(out); got != fx.TriggerID {
		t.Errorf("parent_id = %q, want trigger %q", got, fx.TriggerID)
	}
}

func TestCreateComment_StatusHandoffReplyTriggerJoinsThreadRoot(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	var root string
	fx := newHandoffThreadFixture(t, "status_handoff", func(issueID, a string) string {
		root = dbfx.Comment(t, issueID, "thread root")
		return dbfx.Comment(t, issueID, "handoff note from A", testutil.Cols{"author_type": "agent", "author_id": a, "parent_id": root})
	})
	code, out := fx.post(t, map[string]any{"content": "result of B"})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, out)
	}
	if got := handoffParentOf(out); got != root {
		t.Errorf("parent_id = %q, want thread root %q", got, root)
	}
}

func TestCreateComment_StatusHandoffFullThreadPostsTopLevel(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newHandoffThreadFixture(t, "status_handoff", func(issueID, a string) string {
		root := dbfx.Comment(t, issueID, "thread root")
		for i := 0; i < 28; i++ {
			dbfx.Comment(t, issueID, "reply", testutil.Cols{"parent_id": root})
		}
		// Root + 28 replies + this trigger reply = 30 comments.
		return dbfx.Comment(t, issueID, "handoff note from A", testutil.Cols{"author_type": "agent", "author_id": a, "parent_id": root})
	})
	code, out := fx.post(t, map[string]any{"content": "result of B"})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, out)
	}
	if got := handoffParentOf(out); got != "" {
		t.Errorf("parent_id = %q, want top level", got)
	}
}

// Guard edge: green before and after the change.
func TestCreateComment_CommentTriggeredTaskStillRejectsTopLevel(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newHandoffThreadFixture(t, "comment", func(issueID, a string) string {
		return dbfx.Comment(t, issueID, "member question")
	})
	code, out := fx.post(t, map[string]any{"content": "answer"})
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %v", code, out)
	}
}

// Guard edge: green before and after the change.
func TestCreateComment_StatusHandoffForeignExplicitParentStillRejected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newHandoffThreadFixture(t, "status_handoff", func(issueID, a string) string {
		return dbfx.Comment(t, issueID, "handoff note from A", testutil.Cols{"author_type": "agent", "author_id": a})
	})
	foreign := dbfx.Comment(t, fx.IssueID, "unrelated thread")
	code, out := fx.post(t, map[string]any{"content": "result of B", "parent_id": foreign})
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %v", code, out)
	}
}

func handoffQueuedRunCount(t *testing.T, issueID, agentID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued','dispatched')`,
		issueID, agentID).Scan(&n); err != nil {
		t.Fatalf("count queued tasks: %v", err)
	}
	return n
}

// A derived reply under agent A's comment must start exactly what the same
// comment at top level starts: nothing for A.
func TestCreateComment_StatusHandoffDerivedReplyRoutesLikeTopLevel(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newHandoffThreadFixture(t, "status_handoff", func(issueID, a string) string {
		return dbfx.Comment(t, issueID, "handoff note from A", testutil.Cols{"author_type": "agent", "author_id": a})
	})
	before := handoffQueuedRunCount(t, fx.IssueID, fx.AssigneeID)
	code, out := fx.post(t, map[string]any{"content": "result of B"})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, out)
	}
	if got := handoffParentOf(out); got != fx.TriggerID {
		t.Fatalf("parent_id = %q, want trigger %q", got, fx.TriggerID)
	}
	if after := handoffQueuedRunCount(t, fx.IssueID, fx.AssigneeID); after != before {
		t.Errorf("runs queued for agent A = %d, want %d (same as top level)", after, before)
	}
	if outcomes, _ := out["trigger_outcomes"].([]any); len(outcomes) != 0 {
		t.Errorf("trigger_outcomes = %v, want none (same as top level)", outcomes)
	}
}
