package handler

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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

// CHE-3702 round 12: a fork and a continuation also start a fresh session seeded
// with another DM's content (a copied transcript, or its summary). The source
// DM's tainted turns reach the new task, and so a delegate of it.
func TestChatClaimForkAndContinuationSeedInheritTaintAndReachDelegate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		link     string
		seedKind string
		seedBody string
	}{
		{name: "fork", link: "forked_from_session_id"},
		{name: "continuation", link: "continued_from_session_id", seedKind: "continuation_summary", seedBody: "summary of attacker-influenced content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
			sourceID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "taint source " + tc.name})
			dbfx.Task(t, agentID, testutil.Cols{
				"status": "completed", "runtime_id": runtimeID, "chat_session_id": sourceID,
				"session_id": "sess-tainted", "taint_ids": []string{"t1"},
				"started_at": testutil.Raw("now() - interval '1 minute'"), "completed_at": testutil.Raw("now()"),
			})
			dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sourceID, "role": "assistant", "content": "attacker-influenced reply"})

			// taint_ids is what SetChatForkSource / SetChatContinuationSource store at copy time.
			childID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "taint " + tc.name, tc.link: sourceID, "taint_ids": []string{"t1"}})
			if tc.name == "fork" {
				dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": childID, "role": "assistant", "content": "attacker-influenced reply"})
			} else {
				dbfx.Insert(t, "chat_message", testutil.Cols{
					"chat_session_id": childID, "role": "assistant", "message_kind": tc.seedKind, "content": tc.seedBody,
				})
			}
			dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": childID, "role": "user", "content": "a later, clean message"})
			dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": childID, "priority": 1000})

			dm := claimChatTaintTask(t, runtimeID)
			wantChatTaint(t, dm, "")

			delegateRuntime := createClaimReclaimRuntime(t, ctx, "Seed taint delegate runtime "+tc.name)
			delegateAgent, issueID := createClaimReclaimAgentAndIssue(t, ctx, delegateRuntime, "Seed taint delegate agent "+tc.name)
			insertGrant(t, issueID, testWorkspaceID, []string{"docker"}, "1 minute", "1 hour", false)
			taskID := createDispatchedClaimFixtureTask(t, ctx, delegateAgent, delegateRuntime, issueID, "120 seconds", false)
			dbfx.Exec(t, `UPDATE agent_task_queue SET delegated_from_task_id = $2 WHERE id = $1`, taskID, dm.ID)

			claimRestrictedWithTaint(t, delegateRuntime, []string{"t1"})
		})
	}
}

// CHE-3702 round 13: a taint-free middle link (a fork made before any task ran)
// must not cut the chain. The middle chat carries the root's stored taint, so the
// leaf copies it from the middle, and a delegate of the seeded task claims
// restricted.
func TestChatClaimSeedTaintWalksTaskLessAncestry(t *testing.T) {
	for _, tc := range []struct{ name, lastLink string }{
		{name: "fork of fork", lastLink: "forked_from_session_id"},
		{name: "continuation of fork", lastLink: "continued_from_session_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
			rootID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "ancestry root"})
			dbfx.Task(t, agentID, testutil.Cols{
				"status": "completed", "runtime_id": runtimeID, "chat_session_id": rootID,
				"session_id": "sess-tainted", "taint_ids": []string{"t1"},
				"started_at": testutil.Raw("now() - interval '1 minute'"), "completed_at": testutil.Raw("now()"),
			})
			dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": rootID, "role": "assistant", "content": "attacker-influenced reply"})
			middleID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "ancestry middle, no task", "forked_from_session_id": rootID, "taint_ids": []string{"t1"}})
			dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": middleID, "role": "assistant", "content": "attacker-influenced reply"})
			leafID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "ancestry leaf", tc.lastLink: middleID, "taint_ids": []string{"t1"}})
			kind := testutil.Cols{"chat_session_id": leafID, "role": "assistant", "content": "attacker-influenced reply"}
			if tc.lastLink == "continued_from_session_id" {
				kind["message_kind"] = "continuation_summary"
			}
			dbfx.Insert(t, "chat_message", kind)
			dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": leafID, "role": "user", "content": "a later, clean message"})
			dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": leafID, "priority": 1000})

			dm := claimChatTaintTask(t, runtimeID)
			wantChatTaint(t, dm, "")

			delegateRuntime := createClaimReclaimRuntime(t, ctx, "Ancestry delegate runtime "+tc.name)
			delegateAgent, issueID := createClaimReclaimAgentAndIssue(t, ctx, delegateRuntime, "Ancestry delegate agent "+tc.name)
			insertGrant(t, issueID, testWorkspaceID, []string{"docker"}, "1 minute", "1 hour", false)
			taskID := createDispatchedClaimFixtureTask(t, ctx, delegateAgent, delegateRuntime, issueID, "120 seconds", false)
			dbfx.Exec(t, `UPDATE agent_task_queue SET delegated_from_task_id = $2 WHERE id = $1`, taskID, dm.ID)

			claimRestrictedWithTaint(t, delegateRuntime, []string{"t1"})
		})
	}
}

func forkDM(t *testing.T, sourceID, key, message string) string {
	t.Helper()
	extra := map[string]any{}
	if message != "" {
		extra["message"] = message
	}
	var child ChatForkResponse
	testutil.Call(t, testHandler.ForkChatSession, forkChatRequest(t, sourceID, key, extra)).Want(http.StatusCreated).JSON(&child)
	cleanupChatFork(t, child.Session.ID)
	return child.Session.ID
}

func deleteDM(t *testing.T, sessionID string) {
	t.Helper()
	req := withChatTestWorkspaceCtx(t, withURLParams(newRequest(http.MethodDelete, "/api/chat/sessions/"+sessionID, nil), "sessionId", sessionID))
	testutil.Call(t, testHandler.DeleteChatSession, req).Want(http.StatusNoContent)
}

// CHE-3702 round 14: a fork copies the source transcript, so the copy keeps
// the source's taint after the source is deleted. DeleteChatSession detaches
// the source's tasks from the chat, so the taint has to live on the chat that
// holds the copy. The claim of a later turn must stay restricted, and so must
// a delegate of it.
func TestChatClaimSeedTaintSurvivesSourceDeletion(t *testing.T) {
	for _, tc := range []struct {
		name         string
		middleLinks  bool
		deleteMiddle bool
	}{
		{name: "delete the source of a fork"},
		{name: "fork of fork, delete the source and the middle", middleLinks: true, deleteMiddle: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			agentID, rootID, runtimeID, _ := setupDirectChatSession(t, ctx, "Taint root")
			dbfx.Task(t, agentID, testutil.Cols{
				"status": "completed", "runtime_id": runtimeID, "chat_session_id": rootID,
				"session_id": "sess-tainted", "taint_ids": []string{"t1"},
				"started_at": testutil.Raw("now() - interval '1 minute'"), "completed_at": testutil.Raw("now()"),
			})
			forkMessage(t, rootID, testutil.Cols{"role": "user", "content": "a question", "created_at": testutil.Raw("now() - interval '2 minutes'")})
			forkMessage(t, rootID, testutil.Cols{"role": "assistant", "content": "attacker-influenced reply", "created_at": testutil.Raw("now() - interval '1 minute'")})

			leafID := ""
			if tc.middleLinks {
				middleID := forkDM(t, rootID, "middle", "")
				leafID = forkDM(t, middleID, "leaf", "Next step")
				deleteDM(t, rootID)
				deleteDM(t, middleID)
			} else {
				leafID = forkDM(t, rootID, "leaf", "Next step")
				deleteDM(t, rootID)
			}
			if got := dbfx.Count(t, `SELECT count(*) FROM chat_session WHERE id = $1`, leafID); got != 1 {
				t.Fatalf("leaf DM missing after the deletes: %d rows", got)
			}

			dm := claimChatTaintTask(t, runtimeID)
			wantChatTaint(t, dm, "")

			delegateRuntime := createClaimReclaimRuntime(t, ctx, "Deleted-source delegate runtime "+tc.name)
			delegateAgent, issueID := createClaimReclaimAgentAndIssue(t, ctx, delegateRuntime, "Deleted-source delegate agent "+tc.name)
			insertGrant(t, issueID, testWorkspaceID, []string{"docker"}, "1 minute", "1 hour", false)
			taskID := createDispatchedClaimFixtureTask(t, ctx, delegateAgent, delegateRuntime, issueID, "120 seconds", false)
			dbfx.Exec(t, `UPDATE agent_task_queue SET delegated_from_task_id = $2 WHERE id = $1`, taskID, dm.ID)

			claimRestrictedWithTaint(t, delegateRuntime, []string{"t1"})
		})
	}
}

// SetChatContinuationSource stores the source's taint on the successor, from the
// source chat and from its tasks.
func TestSetChatContinuationSourceStoresSourceTaint(t *testing.T) {
	ctx := context.Background()
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
	sourceID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "continuation source", "taint_ids": []string{"t2"}})
	dbfx.Task(t, agentID, testutil.Cols{
		"status": "completed", "runtime_id": runtimeID, "chat_session_id": sourceID,
		"taint_ids":  []string{"t1"},
		"started_at": testutil.Raw("now() - interval '1 minute'"), "completed_at": testutil.Raw("now()"),
	})
	childID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "continuation child"})
	if _, err := testHandler.Queries.SetChatContinuationSource(ctx, db.SetChatContinuationSourceParams{
		ID: parseUUID(childID), ContinuedFromSessionID: parseUUID(sourceID), SummaryCreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	var stored []string
	dbfx.QueryRow(t, `SELECT taint_ids FROM chat_session WHERE id = $1`, childID).Scan(&stored)
	if !reflect.DeepEqual(stored, []string{"t1", "t2"}) {
		t.Fatalf("stored chat taint = %v, want [t1 t2]", stored)
	}
}
