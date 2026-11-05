package handler

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-3702 round 8: a fired watch stamps its taint on its own DM task only.
// A later DM task resumes the same transcript, so it must carry that taint
// through the claim, or it could hand attacker-influenced content on to a
// delegate past the elevation fence. These tests drive both chat resume paths
// and a delegation from the later, otherwise clean, task.

type chatResumeTaintClaim struct {
	ID             string   `json:"id"`
	PriorSessionID string   `json:"prior_session_id"`
	Profile        string   `json:"profile"`
	TaintIDs       []string `json:"taint_ids"`
}

func claimChatTaintTask(t *testing.T, runtimeID string) chatResumeTaintClaim {
	t.Helper()
	req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/claim", nil), "runtimeId", runtimeID)
	var resp struct {
		Task *chatResumeTaintClaim `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK).JSON(&resp)
	if resp.Task == nil {
		t.Fatal("expected a claimed chat task")
	}
	return *resp.Task
}

func wantChatTaint(t *testing.T, got chatResumeTaintClaim, wantSession string) {
	t.Helper()
	if got.PriorSessionID != wantSession {
		t.Fatalf("PriorSessionID = %q, want %q", got.PriorSessionID, wantSession)
	}
	if got.Profile != "restricted" || !reflect.DeepEqual(got.TaintIDs, []string{"t1"}) {
		t.Fatalf("claim = %+v, want restricted with taint [t1] from the resumed transcript", got)
	}
	var stored []string
	dbfx.QueryRow(t, `SELECT taint_ids FROM agent_task_queue WHERE id = $1`, got.ID).Scan(&stored)
	if !reflect.DeepEqual(stored, []string{"t1"}) {
		t.Fatalf("stored task taint = %v, want [t1]", stored)
	}
}

// Path 1: the chat's resume pointer names the session.
func TestChatClaimResumingPointerSessionInheritsTaint(t *testing.T) {
	ctx := context.Background()
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
	chatSessionID := dbfx.ChatSession(t, agentID, testutil.Cols{
		"title": "taint pointer", "session_id": "sess-tainted", "work_dir": "/tainted", "runtime_id": runtimeID,
	})
	dbfx.Task(t, agentID, testutil.Cols{
		"status": "completed", "runtime_id": runtimeID, "chat_session_id": chatSessionID,
		"session_id": "sess-tainted", "taint_ids": []string{"t1"},
		"started_at": testutil.Raw("now() - interval '1 minute'"), "completed_at": testutil.Raw("now()"),
	})
	dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": chatSessionID, "role": "user", "content": "a later, clean message"})
	dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": chatSessionID, "priority": 1000})

	wantChatTaint(t, claimChatTaintTask(t, runtimeID), "sess-tainted")
}

// Path 2: no pointer, so the claim falls back to the last task row that
// recorded a session.
func TestChatClaimResumingFallbackSessionInheritsTaint(t *testing.T) {
	ctx := context.Background()
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
	chatSessionID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "taint fallback"})
	dbfx.Task(t, agentID, testutil.Cols{
		"status": "completed", "runtime_id": runtimeID, "chat_session_id": chatSessionID,
		"session_id": "sess-tainted", "taint_ids": []string{"t1"},
		"started_at": testutil.Raw("now() - interval '1 minute'"), "completed_at": testutil.Raw("now()"),
	})
	dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": chatSessionID, "role": "user", "content": "a later, clean message"})
	dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": chatSessionID, "priority": 1000})

	wantChatTaint(t, claimChatTaintTask(t, runtimeID), "sess-tainted")
}

// A delegate of the later DM task inherits the taint and claims restricted
// even with a live grant on its issue.
func TestChatClaimTaintReachesDelegateOfLaterTask(t *testing.T) {
	ctx := context.Background()
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
	chatSessionID := dbfx.ChatSession(t, agentID, testutil.Cols{
		"title": "taint delegate", "session_id": "sess-tainted", "work_dir": "/tainted", "runtime_id": runtimeID,
	})
	dbfx.Task(t, agentID, testutil.Cols{
		"status": "completed", "runtime_id": runtimeID, "chat_session_id": chatSessionID,
		"session_id": "sess-tainted", "taint_ids": []string{"t1"},
		"started_at": testutil.Raw("now() - interval '1 minute'"), "completed_at": testutil.Raw("now()"),
	})
	dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": chatSessionID, "role": "user", "content": "a later, clean message"})
	dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": chatSessionID, "priority": 1000})
	dm := claimChatTaintTask(t, runtimeID)
	wantChatTaint(t, dm, "sess-tainted")

	delegateRuntime := createClaimReclaimRuntime(t, ctx, "Taint delegate runtime")
	delegateAgent, issueID := createClaimReclaimAgentAndIssue(t, ctx, delegateRuntime, "Taint delegate agent")
	insertGrant(t, issueID, testWorkspaceID, []string{"docker"}, "1 minute", "1 hour", false)
	taskID := createDispatchedClaimFixtureTask(t, ctx, delegateAgent, delegateRuntime, issueID, "120 seconds", false)
	dbfx.Exec(t, `UPDATE agent_task_queue SET delegated_from_task_id = $2 WHERE id = $1`, taskID, dm.ID)

	claimRestrictedWithTaint(t, delegateRuntime, []string{"t1"})
}

// CHE-3702 round 11: an in-place /compact drops the resume pointer, so the next
// task starts a fresh session seeded from the summary. The summary was written
// by a tainted turn, so that task carries the taint too, and so does a delegate
// of it.
func TestChatClaimCompactionSeedInheritsTaintAndReachesDelegate(t *testing.T) {
	ctx := context.Background()
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
	chatSessionID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "taint compaction", "compaction_epoch": 1})
	compactTask := dbfx.Task(t, agentID, testutil.Cols{
		"status": "completed", "runtime_id": runtimeID, "chat_session_id": chatSessionID,
		"session_id": "sess-tainted", "taint_ids": []string{"t1"},
		"started_at": testutil.Raw("now() - interval '1 minute'"), "completed_at": testutil.Raw("now()"),
	})
	dbfx.Insert(t, "chat_message", testutil.Cols{
		"chat_session_id": chatSessionID, "role": "assistant", "message_kind": "compaction_summary",
		"task_id": compactTask, "content": "summary of attacker-influenced content",
	})
	dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": chatSessionID, "role": "user", "content": "a later, clean message"})
	dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": chatSessionID, "priority": 1000})

	dm := claimChatTaintTask(t, runtimeID)
	wantChatTaint(t, dm, "")

	delegateRuntime := createClaimReclaimRuntime(t, ctx, "Compaction taint delegate runtime")
	delegateAgent, issueID := createClaimReclaimAgentAndIssue(t, ctx, delegateRuntime, "Compaction taint delegate agent")
	insertGrant(t, issueID, testWorkspaceID, []string{"docker"}, "1 minute", "1 hour", false)
	taskID := createDispatchedClaimFixtureTask(t, ctx, delegateAgent, delegateRuntime, issueID, "120 seconds", false)
	dbfx.Exec(t, `UPDATE agent_task_queue SET delegated_from_task_id = $2 WHERE id = $1`, taskID, dm.ID)

	claimRestrictedWithTaint(t, delegateRuntime, []string{"t1"})
}
