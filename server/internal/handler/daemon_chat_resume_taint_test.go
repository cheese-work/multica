package handler

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
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

// CHE-3702 round 15: during a schema-preserving rollback the predecessor binary
// forks without writing chat_session.taint_ids, and migration 974's backfill
// has already run. After the re-upgrade the claim must still find the taint on
// the live source chain, and repair the stored column so a later delete of the
// source cannot lose it.
func TestChatClaimSeedTaintRepairsRowsWrittenByThePredecessor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		link     string
		seedKind string
		deep     bool
	}{
		{name: "fork", link: "forked_from_session_id"},
		{name: "continuation", link: "continued_from_session_id", seedKind: "continuation_summary"},
		{name: "fork of fork", link: "forked_from_session_id", deep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
			rootID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "predecessor root"})
			dbfx.Task(t, agentID, testutil.Cols{
				"status": "completed", "runtime_id": runtimeID, "chat_session_id": rootID,
				"session_id": "sess-tainted", "taint_ids": []string{"t1"},
				"started_at": testutil.Raw("now() - interval '1 minute'"), "completed_at": testutil.Raw("now()"),
			})
			dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": rootID, "role": "assistant", "content": "attacker-influenced reply"})
			parentID := rootID
			if tc.deep {
				// The predecessor wrote every link with an empty taint_ids.
				parentID = dbfx.ChatSession(t, agentID, testutil.Cols{"title": "predecessor middle", tc.link: rootID})
				dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": parentID, "role": "assistant", "content": "attacker-influenced reply"})
			}
			leafID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "predecessor leaf", tc.link: parentID})
			seed := testutil.Cols{"chat_session_id": leafID, "role": "assistant", "content": "attacker-influenced reply"}
			if tc.seedKind != "" {
				seed["message_kind"] = tc.seedKind
			}
			dbfx.Insert(t, "chat_message", seed)
			dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": leafID, "role": "user", "content": "a later, clean message"})
			dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": leafID, "priority": 1000})

			dm := claimChatTaintTask(t, runtimeID)
			wantChatTaint(t, dm, "")

			var stored []string
			dbfx.QueryRow(t, `SELECT taint_ids FROM chat_session WHERE id = $1`, leafID).Scan(&stored)
			if !reflect.DeepEqual(stored, []string{"t1"}) {
				t.Fatalf("stored chat taint after the claim = %v, want [t1] (repair on read)", stored)
			}

			// The repaired column keeps the taint once the source chain is gone.
			dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'failed', completed_at = now() WHERE id = $1`, dm.ID)
			dbfx.Exec(t, `UPDATE agent_task_queue SET chat_session_id = NULL WHERE chat_session_id = ANY($1)`, []string{rootID, parentID})
			dbfx.Exec(t, `DELETE FROM chat_session WHERE id = ANY($1)`, []string{rootID, parentID})
			dbfx.Exec(t, `UPDATE agent_task_queue SET chat_session_id = NULL WHERE chat_session_id = $1 AND id = $2`, leafID, dm.ID)
			dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": leafID, "priority": 1000})
			wantChatTaint(t, claimChatTaintTask(t, runtimeID), "")
		})
	}
}

const unrecoverableTaint = "unrecoverable-ancestry"

// unrecoverableTaintOf is the id a chat that lost its source carries.
func unrecoverableTaintOf(chatID string) string { return unrecoverableTaint + ":" + chatID }

// CHE-3702 round 16: a fork written by the predecessor binary has empty
// taint_ids and no verified mark. If its source is deleted before the first
// claim after re-upgrade, nothing can say what the copy held, so the claim
// fails closed: restricted, with a synthetic taint that a delegate inherits.
func TestChatClaimSeedFailsClosedWhenPredecessorForkLostItsSource(t *testing.T) {
	for _, tc := range []struct {
		name     string
		link     string
		seedKind string
	}{
		{name: "fork", link: "forked_from_session_id"},
		{name: "continuation", link: "continued_from_session_id", seedKind: "continuation_summary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
			rootID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "predecessor source"})
			dbfx.Task(t, agentID, testutil.Cols{
				"status": "completed", "runtime_id": runtimeID, "chat_session_id": rootID,
				"session_id": "sess-tainted", "taint_ids": []string{"t1"},
				"started_at": testutil.Raw("now() - interval '1 minute'"), "completed_at": testutil.Raw("now()"),
			})
			dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": rootID, "role": "assistant", "content": "attacker-influenced reply"})
			leafID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "predecessor fork", tc.link: rootID})
			seed := testutil.Cols{"chat_session_id": leafID, "role": "assistant", "content": "attacker-influenced reply"}
			if tc.seedKind != "" {
				seed["message_kind"] = tc.seedKind
			}
			dbfx.Insert(t, "chat_message", seed)
			dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": leafID, "role": "user", "content": "a later, clean message"})

			// The source is deleted while the predecessor binary is still serving.
			dbfx.Exec(t, `UPDATE agent_task_queue SET chat_session_id = NULL WHERE chat_session_id = $1`, rootID)
			dbfx.Exec(t, `DELETE FROM chat_session WHERE id = $1`, rootID)

			dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": leafID, "priority": 1000})
			dm := claimChatTaintTask(t, runtimeID)
			if dm.Profile != "restricted" || !reflect.DeepEqual(dm.TaintIDs, []string{unrecoverableTaintOf(leafID)}) {
				t.Fatalf("claim = %+v, want restricted with taint [%s]", dm, unrecoverableTaintOf(leafID))
			}

			delegateRuntime := createClaimReclaimRuntime(t, ctx, "Unrecoverable taint delegate runtime")
			delegateAgent, issueID := createClaimReclaimAgentAndIssue(t, ctx, delegateRuntime, "Unrecoverable taint delegate agent")
			insertGrant(t, issueID, testWorkspaceID, []string{"docker"}, "1 minute", "1 hour", false)
			taskID := createDispatchedClaimFixtureTask(t, ctx, delegateAgent, delegateRuntime, issueID, "120 seconds", false)
			dbfx.Exec(t, `UPDATE agent_task_queue SET delegated_from_task_id = $2 WHERE id = $1`, taskID, dm.ID)
			claimRestrictedWithTaint(t, delegateRuntime, []string{unrecoverableTaintOf(leafID)})
		})
	}
}

// A fork of an unlinked chat is complete, a fork of an unverified linked chat
// is left for the claim to resolve, and the claim verifies what it repaired.
func TestChatForkTaintVerifiedFollowsTheSource(t *testing.T) {
	ctx := context.Background()
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
	verified := func(id string) bool {
		var v bool
		dbfx.QueryRow(t, `SELECT taint_verified FROM chat_session WHERE id = $1`, id).Scan(&v)
		return v
	}
	rootID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "verified root"})
	dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": rootID, "role": "user", "content": "hi"})
	forkID := forkDM(t, rootID, "verified-1", "")
	if !verified(forkID) {
		t.Fatal("fork of an unlinked chat must be verified")
	}

	predecessorID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "predecessor fork", "forked_from_session_id": rootID})
	dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": predecessorID, "role": "user", "content": "hi"})
	if verified(predecessorID) {
		t.Fatal("a row written without the column must stay unverified")
	}
	childID := forkDM(t, predecessorID, "verified-2", "")
	if verified(childID) {
		t.Fatal("fork of an unverified linked chat must stay unverified")
	}

	dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": predecessorID, "priority": 1000})
	claimChatTaintTask(t, runtimeID)
	if !verified(predecessorID) {
		t.Fatal("a repaired chat must be verified")
	}
}

// The synthetic taint names the orphan, so a member clearing one orphan on an
// issue does not suppress an unrelated orphan's taint on that issue.
func TestUnrecoverableTaintIsPerOrphanAndClearsIndependently(t *testing.T) {
	ctx := context.Background()
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
	issueRuntime := createClaimReclaimRuntime(t, ctx, "Orphan clear runtime")
	issueAgent, issueID := createClaimReclaimAgentAndIssue(t, ctx, issueRuntime, "Orphan clear agent")

	orphan := func(title string) (leafID string, taint []string) {
		sourceID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": title + " source"})
		leafID = dbfx.ChatSession(t, agentID, testutil.Cols{"title": title, "forked_from_session_id": sourceID})
		dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": leafID, "role": "user", "content": "copied history"})
		dbfx.Exec(t, `DELETE FROM chat_session WHERE id = $1`, sourceID)
		dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": leafID, "priority": 1000})
		dm := claimChatTaintTask(t, runtimeID)
		return leafID, dm.TaintIDs
	}
	leafA, taintA := orphan("orphan A")
	leafB, taintB := orphan("orphan B")
	wantA, wantB := unrecoverableTaintOf(leafA), unrecoverableTaintOf(leafB)
	if !reflect.DeepEqual(taintA, []string{wantA}) || !reflect.DeepEqual(taintB, []string{wantB}) {
		t.Fatalf("taints = %v and %v, want [%s] and [%s]", taintA, taintB, wantA, wantB)
	}

	dbfx.Task(t, issueAgent, testutil.Cols{"status": "completed", "runtime_id": issueRuntime, "issue_id": issueID, "taint_ids": taintA})
	postClearIssueTaint(t, issueID, map[string]any{"taint_ids": taintA}, nil).Want(http.StatusOK)
	dbfx.Task(t, issueAgent, testutil.Cols{"status": "completed", "runtime_id": issueRuntime, "issue_id": issueID, "taint_ids": taintB})
	if got := issueTaintOf(t, issueID); !reflect.DeepEqual(got, []string{wantB}) {
		t.Fatalf("issue taint = %v after clearing A then B arrived, want [%s]", got, wantB)
	}

	// A descendant of the same orphan shares its id.
	childID := forkDM(t, leafA, "orphan-child", "")
	dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": childID, "priority": 1000})
	if dm := claimChatTaintTask(t, runtimeID); !reflect.DeepEqual(dm.TaintIDs, []string{wantA}) {
		t.Fatalf("descendant taint = %v, want [%s]", dm.TaintIDs, wantA)
	}
}

// delegateClaimsRestricted delegates from taskID to a clean issue with a live
// grant and asserts the delegate claims restricted with the wanted taint.
func delegateClaimsRestricted(t *testing.T, taskID, label string, want []string) {
	t.Helper()
	ctx := context.Background()
	delegateRuntime := createClaimReclaimRuntime(t, ctx, "Delegate runtime "+label)
	delegateAgent, issueID := createClaimReclaimAgentAndIssue(t, ctx, delegateRuntime, "Delegate agent "+label)
	insertGrant(t, issueID, testWorkspaceID, []string{"docker"}, "1 minute", "1 hour", false)
	delegateID := createDispatchedClaimFixtureTask(t, ctx, delegateAgent, delegateRuntime, issueID, "120 seconds", false)
	dbfx.Exec(t, `UPDATE agent_task_queue SET delegated_from_task_id = $2 WHERE id = $1`, delegateID, taskID)
	claimRestrictedWithTaint(t, delegateRuntime, want)
}

// CHE-3702 round 18: a resumed DM session carries the transcript of a fork or
// continuation. The task rows of that chat are clean, so the stored chat taint
// and the ancestry check have to apply to the resume path too, not only to the
// three seeds.
func TestChatClaimResumeInheritsStoredChatTaintAndReachesDelegate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, agentID string) (chatID string, want []string)
	}{
		{name: "stored taint", setup: func(t *testing.T, agentID string) (string, []string) {
			sourceID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "resume source"})
			return dbfx.ChatSession(t, agentID, testutil.Cols{
				"title": "resume fork", "forked_from_session_id": sourceID, "taint_ids": []string{"t1"}, "taint_verified": true,
			}), []string{"t1"}
		}},
		{name: "missing parent", setup: func(t *testing.T, agentID string) (string, []string) {
			chatID := dbfx.ChatSession(t, agentID, testutil.Cols{
				"title": "resume orphan", "forked_from_session_id": "00000000-0000-4000-8000-0000000000e1",
			})
			return chatID, []string{unrecoverableTaintOf(chatID)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
			chatID, want := tc.setup(t, agentID)
			dbfx.Exec(t, `UPDATE chat_session SET session_id = 'sess-fork', work_dir = '/fork', runtime_id = $2 WHERE id = $1`, chatID, runtimeID)
			dbfx.Task(t, agentID, testutil.Cols{
				"status": "completed", "runtime_id": runtimeID, "chat_session_id": chatID, "session_id": "sess-fork",
				"started_at": testutil.Raw("now() - interval '1 minute'"), "completed_at": testutil.Raw("now()"),
			})
			dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": chatID, "role": "user", "content": "a later, clean message"})
			dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": chatID, "priority": 1000})

			dm := claimChatTaintTask(t, runtimeID)
			if dm.PriorSessionID != "sess-fork" || dm.Profile != "restricted" || !reflect.DeepEqual(dm.TaintIDs, want) {
				t.Fatalf("claim = %+v, want resumed sess-fork, restricted with taint %v", dm, want)
			}
			delegateClaimsRestricted(t, dm.ID, tc.name, want)
		})
	}
}

// CHE-3702 round 18: `multica chat history` hands the chat transcript to the
// reading task, so that task takes the chat's taint before content returns, and
// a delegate of it is restricted.
func TestChatHistoryReadMarksReadingTaskAndReachesDelegate(t *testing.T) {
	ctx := context.Background()
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
	sourceID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "history source"})
	chatID := dbfx.ChatSession(t, agentID, testutil.Cols{
		"title": "history fork", "forked_from_session_id": sourceID, "taint_ids": []string{"t1"}, "taint_verified": true,
	})
	dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": chatID, "role": "assistant", "content": "attacker-influenced reply"})
	taskID := dbfx.Task(t, agentID, testutil.Cols{"status": "running", "runtime_id": runtimeID, "chat_session_id": chatID})

	w := serveChat(t, chatGateRouter(nil), "/api/chat/history?limit=10", taskID)
	if w.Code != http.StatusOK {
		t.Fatalf("history status = %d: %s", w.Code, w.Body.String())
	}
	var stored []string
	dbfx.QueryRow(t, `SELECT taint_ids FROM agent_task_queue WHERE id = $1`, taskID).Scan(&stored)
	if !reflect.DeepEqual(stored, []string{"t1"}) {
		t.Fatalf("reading task taint = %v, want [t1]", stored)
	}
	delegateClaimsRestricted(t, taskID, "history", []string{"t1"})
}

// onceBeforeQuery runs inject once, immediately before the first query whose
// SQL contains match, so a test can commit a row at an exact point in a
// handler's read sequence.
type onceBeforeQuery struct {
	db.DBTX
	match  string
	inject func()
	done   bool
}

func (d *onceBeforeQuery) Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	if !d.done && strings.Contains(sql, d.match) {
		d.done = true
		d.inject()
	}
	return d.DBTX.Query(ctx, sql, args...)
}

// lateReader is a Slack-backed history reader that commits a row right before
// it returns the page, i.e. after the handler decided to read.
type lateReader struct {
	inject func()
	page   channel.HistoryPage
}

func (l *lateReader) ChannelOverview(context.Context, pgtype.UUID, channel.HistoryOptions) (channel.HistoryPage, error) {
	l.inject()
	return l.page, nil
}

func (l *lateReader) Thread(context.Context, pgtype.UUID, string, channel.HistoryOptions) (channel.HistoryPage, error) {
	l.inject()
	return l.page, nil
}

// readMidCommit is the CHE-3702 round 19 interleaving: the chat is clean when a
// history read starts, and a tainted turn (message + task, committed together as
// the watch does) lands after the handler begins but before the transcript is
// read. The response must contain that message, and the reading task must carry
// its taint, so a delegate of the reader is restricted.
func readMidCommit(target string, slackBacked bool) func(*testing.T, string, string) string {
	return func(t *testing.T, agentID, runtimeID string) string {
		chatID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "mid-read commit"})
		const late = "late tainted reply"
		taskID := dbfx.Task(t, agentID, testutil.Cols{"status": "running", "runtime_id": runtimeID, "chat_session_id": chatID})
		inject := func() {
			lateTask := dbfx.Task(t, agentID, testutil.Cols{
				"status": "completed", "runtime_id": runtimeID, "chat_session_id": chatID, "taint_ids": []string{"t1"},
				"started_at": testutil.Raw("now() - interval '1 minute'"), "completed_at": testutil.Raw("now()"),
			})
			dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": chatID, "role": "assistant", "content": late, "task_id": lateTask})
		}
		origQueries, origSlack := testHandler.Queries, testHandler.SlackHistory
		t.Cleanup(func() { testHandler.Queries, testHandler.SlackHistory = origQueries, origSlack })
		if slackBacked {
			testHandler.SlackHistory = &lateReader{inject: inject, page: channel.HistoryPage{
				ChannelType: "slack", Messages: []channel.HistoryMessage{{ID: "1", Role: channel.HistoryRoleAssistant, Text: late, TS: "1"}},
			}}
		} else {
			testHandler.SlackHistory = nil
			testHandler.Queries = db.New(&onceBeforeQuery{DBTX: testPool, match: "name: ListChatMessagesPage :many", inject: inject})
		}
		w := serveChat(t, chatGateRouter(nil), target, taskID)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), late) {
			t.Fatalf("read = %d %s, want 200 containing %q", w.Code, w.Body.String(), late)
		}
		return taskID
	}
}

// CHE-3702 round 18 guard: every path by which chat content enters a task. The
// only taint sits on the chat (chat_session.taint_ids); its task rows are clean,
// so a path that skips the shared helper (recordChatTaint) leaves the task and
// its delegate unrestricted. A new path that reads chat content belongs here.
func TestEveryChatContentPathRestrictsADelegate(t *testing.T) {
	cleanDone := testutil.Cols{
		"status":     "completed",
		"started_at": testutil.Raw("now() - interval '1 minute'"), "completed_at": testutil.Raw("now()"),
	}
	userMsg := func(chatID string) {
		dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": chatID, "role": "user", "content": "a later, clean message"})
	}
	type chatPath struct {
		name string
		run  func(t *testing.T, agentID, runtimeID string) (taskID string)
	}
	claimed := func(cols testutil.Cols, seed func(t *testing.T, agentID, runtimeID, chatID string)) func(*testing.T, string, string) string {
		return func(t *testing.T, agentID, runtimeID string) string {
			cols["taint_ids"] = []string{"t1"}
			cols["taint_verified"] = true
			chatID := dbfx.ChatSession(t, agentID, cols)
			seed(t, agentID, runtimeID, chatID)
			userMsg(chatID)
			dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": chatID, "priority": 1000})
			dm := claimChatTaintTask(t, runtimeID)
			if dm.Profile != "restricted" || !reflect.DeepEqual(dm.TaintIDs, []string{"t1"}) {
				t.Fatalf("claim = %+v, want restricted with taint [t1]", dm)
			}
			return dm.ID
		}
	}
	read := func(target string) func(*testing.T, string, string) string {
		return func(t *testing.T, agentID, runtimeID string) string {
			chatID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "read path", "taint_ids": []string{"t1"}, "taint_verified": true})
			dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": chatID, "role": "assistant", "content": "attacker-influenced reply"})
			taskID := dbfx.Task(t, agentID, testutil.Cols{"status": "running", "runtime_id": runtimeID, "chat_session_id": chatID})
			w := serveChat(t, chatGateRouter(nil), target, taskID)
			if w.Code != http.StatusOK {
				t.Fatalf("read status = %d: %s", w.Code, w.Body.String())
			}
			return taskID
		}
	}
	for _, p := range []chatPath{
		{"resume", claimed(testutil.Cols{"title": "p resume", "session_id": "sess-p", "work_dir": "/p"},
			func(t *testing.T, agentID, runtimeID, chatID string) {
				dbfx.Exec(t, `UPDATE chat_session SET runtime_id = $2 WHERE id = $1`, chatID, runtimeID)
				c := testutil.Cols{"runtime_id": runtimeID, "chat_session_id": chatID, "session_id": "sess-p"}
				for k, v := range cleanDone {
					c[k] = v
				}
				dbfx.Task(t, agentID, c)
			})},
		{"compaction seed", claimed(testutil.Cols{"title": "p compaction", "compaction_epoch": 1},
			func(t *testing.T, agentID, runtimeID, chatID string) {
				c := testutil.Cols{"runtime_id": runtimeID, "chat_session_id": chatID, "session_id": "sess-p"}
				for k, v := range cleanDone {
					c[k] = v
				}
				dbfx.Insert(t, "chat_message", testutil.Cols{
					"chat_session_id": chatID, "role": "assistant", "message_kind": "compaction_summary",
					"task_id": dbfx.Task(t, agentID, c), "content": "summary",
				})
			})},
		{"fork seed", claimed(testutil.Cols{"title": "p fork", "forked_from_session_id": "00000000-0000-4000-8000-0000000000e2"},
			func(t *testing.T, agentID, runtimeID, chatID string) {
				dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": chatID, "role": "assistant", "content": "copied reply"})
			})},
		{"continuation seed", claimed(testutil.Cols{"title": "p continuation", "continued_from_session_id": "00000000-0000-4000-8000-0000000000e3"},
			func(t *testing.T, agentID, runtimeID, chatID string) {
				dbfx.Insert(t, "chat_message", testutil.Cols{
					"chat_session_id": chatID, "role": "assistant", "message_kind": "continuation_summary", "content": "summary",
				})
			})},
		{"supplement claim", func(t *testing.T, _, _ string) string {
			f := steeredFixture(t)
			dbfx.Exec(t, `UPDATE chat_session SET taint_ids = '{t1}', taint_verified = true WHERE id = $1`, f.sessionID)
			claimSteer(t, newChatSupplementDaemonClient(t), f)
			return f.runningID
		}},
		{"history read", read("/api/chat/history?limit=10")},
		{"thread read", read("/api/chat/thread")},
		{"history read, tainted turn commits mid-read", readMidCommit("/api/chat/history?limit=10", false)},
		{"thread read, tainted turn commits mid-read", readMidCommit("/api/chat/thread", false)},
		{"slack history, tainted turn commits mid-read", readMidCommit("/api/chat/history?limit=10", true)},
		{"slack thread, tainted turn commits mid-read", readMidCommit("/api/chat/thread", true)},
	} {
		t.Run(p.name, func(t *testing.T) {
			var agentID, runtimeID string
			// The supplement row builds its own agent (steeredFixture), which would
			// collide on name with the one created here.
			if p.name != "supplement claim" {
				agentID, runtimeID, _ = createRuntimeGuardAgent(t, context.Background())
			}
			delegateClaimsRestricted(t, p.run(t, agentID, runtimeID), p.name, []string{"t1"})
		})
	}
}
