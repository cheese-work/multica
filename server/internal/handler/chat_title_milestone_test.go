package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-4107: a DM is re-titled at two milestones, the first message after a
// compaction and the first message of a fork child. Each trigger queues one
// bg:dm_title job keyed by (session, milestone, source message).

// milestoneFixture returns a DM in the default test workspace whose dm_title
// job points at an online agent that advertises the output-only capability.
func milestoneFixture(t *testing.T, title string) (agentID, sessionID string) {
	t.Helper()
	ctx := context.Background()
	agentID, sessionID, runtimeID, _ := setupDirectChatSession(t, ctx, title)
	dbfx.Exec(t, `UPDATE agent_runtime SET provider='claude', status='online', last_seen_at=now(), metadata=jsonb_build_object('capabilities', jsonb_build_array($2::text)) WHERE id=$1`,
		runtimeID, protocol.DaemonCapabilityBackgroundOutputOnlyV1)
	var prior []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&prior)
	dbfx.Cleanup(t, `UPDATE workspace SET settings=$2 WHERE id=$1`, testWorkspaceID, prior)
	putBG(t, testWorkspaceID, map[string]any{"dm_title": map[string]any{"agent_id": agentID}}, nil).Want(http.StatusOK)
	return agentID, sessionID
}

// compactedMilestoneDM adds a prior exchange and a compaction summary to the DM.
func compactedMilestoneDM(t *testing.T, sessionID string) {
	t.Helper()
	forkMessage(t, sessionID, testutil.Cols{"role": "user", "content": "Plan the schema", "created_at": testutil.Raw("now() - interval '3 minutes'")})
	forkMessage(t, sessionID, testutil.Cols{"role": "assistant", "content": "Use Postgres", "created_at": testutil.Raw("now() - interval '2 minutes'")})
	forkMessage(t, sessionID, testutil.Cols{"role": "assistant", "content": "Summary of the schema talk", "message_kind": "compaction_summary", "created_at": testutil.Raw("now() - interval '1 minute'")})
	dbfx.Exec(t, `UPDATE chat_session SET compaction_epoch = 1 WHERE id = $1`, sessionID)
}

func sendDM(t *testing.T, sessionID, content string) {
	t.Helper()
	req := withChatTestWorkspaceCtx(t, newRequest("POST", "/api/chat/sessions/"+sessionID+"/messages", map[string]any{"content": content}))
	testutil.Call(t, testHandler.SendChatMessage, withURLParam(req, "sessionId", sessionID)).Want(http.StatusCreated)
}

func TestDMTitleCompactionMilestoneQueuesOnFirstMessageAfterCompaction(t *testing.T) {
	requireDB(t)
	_, sid := milestoneFixture(t, "Schema Plan")
	compactedMilestoneDM(t, sid)

	sendDM(t, sid, "Now let's talk about billing")
	tasks := dmTitleTasks(t, sid)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want one bg:dm_title task for the post-compaction message", tasks)
	}
	if !strings.Contains(tasks[0].Input, "Schema Plan") || !strings.Contains(tasks[0].Input, "Now let's talk about billing") {
		t.Fatalf("input lacks the previous title or the new message: %q", tasks[0].Input)
	}
	completeDMTitle(t, tasks[0], "Billing")
	if got := sessionTitle(t, sid); got != "Billing" {
		t.Fatalf("title = %q, want the job output", got)
	}

	sendDM(t, sid, "And invoices?")
	if tasks := dmTitleTasks(t, sid); len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want no new job for a later message in the same epoch", tasks)
	}
}

func TestDMTitleMilestoneSkippedAfterManualRename(t *testing.T) {
	requireDB(t)
	_, sid := milestoneFixture(t, "Schema Plan")
	compactedMilestoneDM(t, sid)
	if _, err := testHandler.Queries.UpdateChatSessionTitle(context.Background(), db.UpdateChatSessionTitleParams{ID: util.MustParseUUID(sid), Title: "My Name"}); err != nil {
		t.Fatal(err)
	}

	sendDM(t, sid, "Now let's talk about billing")
	if tasks := dmTitleTasks(t, sid); len(tasks) != 0 {
		t.Fatalf("tasks = %+v, want none for a manually named DM", tasks)
	}
}

func TestDMTitleMilestoneRenameDuringRunWins(t *testing.T) {
	requireDB(t)
	_, sid := milestoneFixture(t, "Schema Plan")
	compactedMilestoneDM(t, sid)
	sendDM(t, sid, "Now let's talk about billing")
	tasks := dmTitleTasks(t, sid)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want one", tasks)
	}
	if _, err := testHandler.Queries.UpdateChatSessionTitle(context.Background(), db.UpdateChatSessionTitleParams{ID: util.MustParseUUID(sid), Title: "My Name"}); err != nil {
		t.Fatal(err)
	}
	completeDMTitle(t, tasks[0], "Billing")
	if got := sessionTitle(t, sid); got != "My Name" {
		t.Fatalf("title = %q, want the manual rename kept", got)
	}
}

func TestDMTitleForkChildFirstMessageGetsRun(t *testing.T) {
	requireDB(t)
	_, src := milestoneFixture(t, "Schema Plan")
	forkMessage(t, src, testutil.Cols{"role": "user", "content": "Plan the schema", "created_at": testutil.Raw("now() - interval '2 minutes'")})
	forkMessage(t, src, testutil.Cols{"role": "assistant", "content": "Use Postgres", "created_at": testutil.Raw("now() - interval '1 minute'")})

	// First message sent with the fork request.
	var withMessage ChatForkResponse
	testutil.Call(t, testHandler.ForkChatSession, forkChatRequest(t, src, "milestone-msg", map[string]any{"message": "Try SQLite instead"})).Want(http.StatusCreated).JSON(&withMessage)
	cleanupChatFork(t, withMessage.Session.ID)
	tasks := dmTitleTasks(t, withMessage.Session.ID)
	if len(tasks) != 1 || !strings.Contains(tasks[0].Input, "Try SQLite instead") {
		t.Fatalf("tasks = %+v, want one job carrying the fork's first message", tasks)
	}

	// First message sent to an empty fork; the next one does not retrigger.
	var empty ChatForkResponse
	testutil.Call(t, testHandler.ForkChatSession, forkChatRequest(t, src, "milestone-empty")).Want(http.StatusCreated).JSON(&empty)
	cleanupChatFork(t, empty.Session.ID)
	sendDM(t, empty.Session.ID, "First question in the fork")
	if tasks := dmTitleTasks(t, empty.Session.ID); len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want one job for the fork's first send", tasks)
	}
	sendDM(t, empty.Session.ID, "Second question")
	if tasks := dmTitleTasks(t, empty.Session.ID); len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want no job for the second send", tasks)
	}
}

func TestDMTitleMilestoneDuplicateTriggerEnqueuesOnce(t *testing.T) {
	requireDB(t)
	_, sid := milestoneFixture(t, "Schema Plan")
	msgID := util.MustParseUUID(forkMessage(t, sid, testutil.Cols{"role": "user", "content": "Billing"}))

	for range 3 {
		testHandler.enqueueDMTitle(testWorkspaceID, testUserID, util.MustParseUUID(sid), dmTitleCompaction, "Schema Plan", "Billing", msgID)
	}
	if tasks := dmTitleTasks(t, sid); len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want one job for one (session, milestone, message)", tasks)
	}
	other := util.MustParseUUID(forkMessage(t, sid, testutil.Cols{"role": "user", "content": "Invoices"}))
	testHandler.enqueueDMTitle(testWorkspaceID, testUserID, util.MustParseUUID(sid), dmTitleCompaction, "Schema Plan", "Invoices", other)
	if tasks := dmTitleTasks(t, sid); len(tasks) != 2 {
		t.Fatalf("tasks = %+v, want a second job for a different source message", tasks)
	}
}
