package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-1727 review R1-R3: what a pause drops must stay dropped when an admin
// resumes before any scheduler tick has run, and the switch must not be
// clearable by a malformed settings body.

type resumeEnv struct {
	ws, issue, agent string
	svc              *service.IssueWakeupService
}

func newResumeEnv(t *testing.T, slug string) resumeEnv {
	t.Helper()
	ws := pauseWorkspaceFixture(t, slug)
	runtime := dbfx.Runtime(t, slug, testutil.Cols{"workspace_id": ws})
	agent := dbfx.Agent(t, slug, runtime, testutil.Cols{"workspace_id": ws})
	issue := dbfx.Issue(t, slug, testutil.Cols{"workspace_id": ws})
	dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", issue)
	dbfx.Cleanup(t, "DELETE FROM issue_wakeup WHERE issue_id=$1", issue)
	dbfx.Cleanup(t, "DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN(SELECT id FROM issue_wakeup WHERE issue_id=$1)", issue)
	dbfx.Cleanup(t, "DELETE FROM activity_log WHERE issue_id=$1", issue)
	return resumeEnv{ws: ws, issue: issue, agent: agent, svc: &service.IssueWakeupService{Tasks: testHandler.TaskService}}
}

func (e resumeEnv) create(t *testing.T, in service.WakeupInput) db.IssueWakeup {
	t.Helper()
	in.AgentID = e.agent
	w, err := e.svc.Create(context.Background(), parseUUID(e.issue), parseUUID(testUserID), pgtype.UUID{}, in)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func (e resumeEnv) tick(t *testing.T) {
	t.Helper()
	if err := e.svc.TickWorkspaces(context.Background(), parseUUID(e.ws)); err != nil {
		t.Fatal(err)
	}
}

func (e resumeEnv) runs(t *testing.T, w db.IssueWakeup) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE context->>'wakeup_id'=$1`, uuidToString(w.ID)).Scan(&n)
	return n
}

func TestResumeBeforeNextTickDoesNotReplayPausedEvents(t *testing.T) {
	e := newResumeEnv(t, "wakeup-resume-event")
	w := e.create(t, service.WakeupInput{Kind: "event", Mode: "continuous", EventTypes: []string{"comment.created"}, Instruction: "look"})
	saveWorkspaceSettings(t, e.ws, map[string]any{"wakeups_paused": true})
	dbfx.Comment(t, e.issue, "arrives while paused", testutil.Cols{"workspace_id": e.ws})
	// No scheduler tick runs during the pause; the admin resumes right away.
	saveWorkspaceSettings(t, e.ws, map[string]any{"wakeups_paused": false})
	e.tick(t)
	if n := e.runs(t, w); n != 0 {
		t.Fatalf("resume replayed %d events that arrived while paused", n)
	}
	dbfx.Comment(t, e.issue, "arrives after resume", testutil.Cols{"workspace_id": e.ws})
	e.tick(t)
	if n := e.runs(t, w); n != 1 {
		t.Fatalf("an event after resume started %d runs, want 1", n)
	}
}

func TestResumeBeforeNextTickSkipsMissedRecurringSlots(t *testing.T) {
	e := newResumeEnv(t, "wakeup-resume-slots")
	w := e.create(t, service.WakeupInput{Kind: "every", IntervalSeconds: 3600, Instruction: "tick"})
	saveWorkspaceSettings(t, e.ws, map[string]any{"wakeups_paused": true})
	dbfx.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '3 hours' WHERE id=$1", w.ID)
	saveWorkspaceSettings(t, e.ws, map[string]any{"wakeups_paused": false})
	e.tick(t)
	if n := e.runs(t, w); n != 0 {
		t.Fatalf("resume fired %d slots missed during the pause", n)
	}
	var future bool
	dbfx.QueryRow(t, `SELECT next_fire_at>now() FROM issue_wakeup WHERE id=$1`, w.ID).Scan(&future)
	if !future {
		t.Fatal("the next slot must be in the future after resume")
	}
}

func TestMalformedSettingsBodyCannotClearThePause(t *testing.T) {
	ws := pauseWorkspaceFixture(t, "wakeup-pause-malformed")
	saveWorkspaceSettings(t, ws, map[string]any{"wakeups_paused": true})
	for _, body := range []any{[]any{}, "text", 7} {
		req := withURLParam(newRequest("PATCH", "/api/workspaces/"+ws, map[string]any{"settings": body}), "id", ws)
		testutil.Call(t, testHandler.UpdateWorkspace, req).Want(http.StatusBadRequest)
	}
	var paused bool
	dbfx.QueryRow(t, `SELECT COALESCE(settings->>'wakeups_paused'='true',false) FROM workspace WHERE id=$1`, ws).Scan(&paused)
	if !paused {
		t.Fatal("a malformed settings body cleared the pause")
	}
	if n := pauseActivities(t, ws, "wakeups_resumed"); n != 0 {
		t.Fatalf("resume records = %d, want 0", n)
	}
}
