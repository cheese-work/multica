package handler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// A burst of failed runs with one provider error opens one incident issue and
// one notice; later failures add comments; 30 quiet minutes close it (CHE-9892).
func TestIncidentMonitor_OpenCommentNotifyOnceClose(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	project := dbfx.Project(t, "incident project")
	var oldSettings []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&oldSettings)
	dbfx.Exec(t, `UPDATE workspace SET settings = settings || jsonb_build_object('incident_response',
		jsonb_build_object('enabled', true, 'project_id', $2::text, 'notify_user_id', $3::text)) WHERE id = $1`,
		testWorkspaceID, project, testUserID)
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `UPDATE workspace SET settings = $2 WHERE id = $1`, testWorkspaceID, oldSettings)
		_, _ = testPool.Exec(ctx, `DELETE FROM inbox_item WHERE type = 'incident' AND workspace_id = $1`, testWorkspaceID)
		_, _ = testPool.Exec(ctx, `DELETE FROM issue WHERE workspace_id = $1 AND metadata ? 'incident_class'`, testWorkspaceID)
	})
	// Earlier runs of this test must not leak an open incident into this one.
	dbfx.Exec(t, `DELETE FROM issue WHERE workspace_id = $1 AND metadata ? 'incident_class'`, testWorkspaceID)

	agent := dbfx.Agent(t, "incident-agent", testRuntimeID)
	now := time.Now().UTC().Truncate(time.Second)
	fail := func(agoMin int, reason, errText string) string {
		issue := dbfx.Issue(t, fmt.Sprintf("incident victim %d", agoMin))
		return dbfx.Task(t, agent, testutil.Cols{"issue_id": issue, "status": "failed", "failure_reason": reason, "error": errText,
			"completed_at": now.Add(-time.Duration(agoMin) * time.Minute)})
	}
	count := func(sql string) int { return dbfx.Count(t, sql, testWorkspaceID) }
	const incidents = `SELECT count(*) FROM issue WHERE workspace_id = $1 AND metadata->>'incident_class' = 'concurrency_limit'`
	const notices = `SELECT count(*) FROM inbox_item WHERE workspace_id = $1 AND type = 'incident'`

	// A different kind of failure never counts.
	fail(1, "agent_error.process_failure", "exit 1")
	fail(5, "agent_error.unknown", "Concurrency limit exceeded for account")
	fail(4, "agent_error.unknown", "Concurrency limit exceeded for account")
	if testHandler.SweepIncidents(ctx, now); count(incidents) != 0 {
		t.Fatal("two failures opened an incident")
	}
	fail(3, "agent_error.unknown", "Concurrency limit exceeded for account")
	testHandler.SweepIncidents(ctx, now)
	testHandler.SweepIncidents(ctx, now) // a repeat pass must not duplicate anything
	if got := count(incidents); got != 1 {
		t.Fatalf("incident issues = %d, want 1", got)
	}
	if got := count(notices); got != 1 {
		t.Fatalf("notices = %d, want 1", got)
	}
	var issueID, status string
	dbfx.QueryRow(t, `SELECT id::text, status FROM issue WHERE workspace_id = $1 AND metadata->>'incident_class' = 'concurrency_limit'`, testWorkspaceID).Scan(&issueID, &status)
	var priority string
	var gotProject string
	dbfx.QueryRow(t, `SELECT priority, project_id::text FROM issue WHERE id = $1`, issueID).Scan(&priority, &gotProject)
	if priority != "urgent" || gotProject != project {
		t.Fatalf("priority/project = %s/%s, want urgent/%s", priority, gotProject, project)
	}

	// A later failure is a comment on the same incident, not a new issue or notice.
	later := now.Add(10 * time.Minute)
	dbfx.Task(t, agent, testutil.Cols{"issue_id": dbfx.Issue(t, "incident victim late"), "status": "failed",
		"failure_reason": "agent_error.unknown", "error": "Concurrency limit exceeded for account", "completed_at": later.Add(-time.Minute)})
	testHandler.SweepIncidents(ctx, later)
	if count(incidents) != 1 || count(notices) != 1 {
		t.Fatalf("second burst: incidents=%d notices=%d, want 1 and 1", count(incidents), count(notices))
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, issueID); got != 1 {
		t.Fatalf("comments = %d, want 1", got)
	}

	// Quiet for 30 minutes: it closes once.
	testHandler.SweepIncidents(ctx, later.Add(31*time.Minute))
	testHandler.SweepIncidents(ctx, later.Add(41*time.Minute))
	dbfx.QueryRow(t, `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status)
	if status != "done" {
		t.Fatalf("status = %s, want done", status)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, issueID); got != 2 {
		t.Fatalf("comments after close = %d, want 2 (one failure comment, one close comment)", got)
	}
}
