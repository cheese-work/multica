package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// incidentSweepFixture turns incident_response on for the shared test workspace, gives the
// member one DM and seeds three failed runs of one provider error class on three issues
// (the 25/11 shape: same class, 3 runs, 2+ issues). Cleanup restores the workspace.
func incidentSweepFixture(t *testing.T, mode string) (agentID string) {
	return incidentSweepFixtureDM(t, mode, true)
}

// incidentSweepFixtureDM is incidentSweepFixture with the member DM optional: withDM false leaves the
// member with no active DM, the case where the notification has nowhere to go yet.
func incidentSweepFixtureDM(t *testing.T, mode string, withDM bool) (agentID string) {
	t.Helper()
	var previous []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
	dbfx.Exec(t, `UPDATE workspace SET settings = settings || jsonb_build_object('incident_response',
		jsonb_build_object('mode', $2::text, 'notify_user_id', $3::text)) WHERE id = $1`, testWorkspaceID, mode, testUserID)
	agentID = createHandlerTestAgent(t, "incident sweep agent", nil)
	if withDM {
		seedIncidentDM(t, agentID)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM issue WHERE workspace_id = $1 AND metadata ->> 'incident_class' IS NOT NULL`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM chat_session WHERE workspace_id = $1 AND creator_id = $2 AND title = 'incident dm'`, testWorkspaceID, testUserID)
		testPool.Exec(ctx, `UPDATE workspace SET settings = $2 WHERE id = $1`, testWorkspaceID, previous)
	})
	for _, title := range []string{"a", "b", "c"} {
		issueID := dbfx.Issue(t, "incident victim "+title)
		dbfx.Task(t, agentID, testutil.Cols{
			"runtime_id":   handlerTestRuntimeID(t),
			"issue_id":     issueID,
			"status":       "failed",
			"error":        "Concurrency limit exceeded for account",
			"completed_at": testutil.Raw("now()"),
		})
	}
	// dbfx.Issue numbers by MAX+1; CreateIssue draws from issue_counter, so sync it.
	dbfx.Exec(t, `UPDATE workspace SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) WHERE id = $1`, testWorkspaceID)
	return agentID
}

func seedIncidentDM(t *testing.T, agentID string) string {
	t.Helper()
	var sessionID string
	dbfx.QueryRow(t, `INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status)
		VALUES ($1, $2, $3, 'incident dm', 'active') RETURNING id`, testWorkspaceID, agentID, testUserID).Scan(&sessionID)
	return sessionID
}

func incidentCounts(t *testing.T) (issues, dms int) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT count(*) FROM issue WHERE workspace_id = $1 AND metadata ->> 'incident_class' = 'concurrency_limit'`, testWorkspaceID).Scan(&issues)
	dbfx.QueryRow(t, `SELECT count(*) FROM chat_message m JOIN chat_session s ON s.id = m.chat_session_id
		WHERE s.workspace_id = $1 AND s.creator_id = $2 AND m.sent_via = 'system' AND m.content LIKE 'Provider incident `+"`concurrency_limit`"+`%'`,
		testWorkspaceID, testUserID).Scan(&dms)
	return issues, dms
}

func TestSweepIncidentsOpensOneIncidentAndOneDM(t *testing.T) {
	incidentSweepFixture(t, "act")
	for sweep := 1; sweep <= 2; sweep++ {
		if err := testHandler.SweepIncidents(context.Background()); err != nil {
			t.Fatalf("sweep %d: %v", sweep, err)
		}
		if issues, dms := incidentCounts(t); issues != 1 || dms != 1 {
			t.Fatalf("after sweep %d: %d incident issues and %d DMs, want 1 and 1", sweep, issues, dms)
		}
	}
	var priority string
	dbfx.QueryRow(t, `SELECT priority FROM issue WHERE workspace_id = $1 AND metadata ->> 'incident_class' = 'concurrency_limit'`, testWorkspaceID).Scan(&priority)
	if priority != "urgent" {
		t.Fatalf("incident priority = %q, want urgent", priority)
	}
}

func TestSweepIncidentsShadowModeChangesNothing(t *testing.T) {
	incidentSweepFixture(t, "shadow")
	if err := testHandler.SweepIncidents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if issues, dms := incidentCounts(t); issues != 0 || dms != 0 {
		t.Fatalf("shadow mode wrote %d issues and %d DMs, want 0 and 0", issues, dms)
	}
}

func TestSweepIncidentsClosesAfterQuietWindow(t *testing.T) {
	incidentSweepFixture(t, "act")
	if err := testHandler.SweepIncidents(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Age every failure and the incident's own record past the 30 minute window.
	dbfx.Exec(t, `UPDATE agent_task_queue SET completed_at = now() - interval '2 hours' WHERE issue_id IN (SELECT id FROM issue WHERE workspace_id = $1 AND title LIKE 'incident victim %')`, testWorkspaceID)
	dbfx.Exec(t, `UPDATE issue SET metadata = metadata || jsonb_build_object('incident_last_failure_at', to_char(now() - interval '2 hours', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
		WHERE workspace_id = $1 AND metadata ->> 'incident_class' = 'concurrency_limit'`, testWorkspaceID)
	if err := testHandler.SweepIncidents(context.Background()); err != nil {
		t.Fatal(err)
	}
	var status string
	var closing int
	dbfx.QueryRow(t, `SELECT status FROM issue WHERE workspace_id = $1 AND metadata ->> 'incident_class' = 'concurrency_limit'`, testWorkspaceID).Scan(&status)
	dbfx.QueryRow(t, `SELECT count(*) FROM comment c JOIN issue i ON i.id = c.issue_id WHERE i.metadata ->> 'incident_class' = 'concurrency_limit' AND c.content LIKE 'Closed:%'`).Scan(&closing)
	if status != "done" || closing != 1 {
		t.Fatalf("after the quiet window: status %q, %d closing comments, want done and 1", status, closing)
	}
}

func TestValidateIncidentResponseSetting(t *testing.T) {
	const uuid = "8f4d3a52-2b6e-4f0a-9d3e-5a1c7b9e0f11"
	cases := []struct {
		name    string
		value   any
		wantErr bool
	}{
		{"absent key", map[string]any{"other": 1}, false},
		{"not an object settings", "x", false},
		{"off needs nothing", map[string]any{"incident_response": map[string]any{"mode": "off"}}, false},
		{"shadow needs nothing", map[string]any{"incident_response": map[string]any{"mode": "shadow"}}, false},
		{"act with notify user", map[string]any{"incident_response": map[string]any{"mode": "act", "notify_user_id": uuid, "project_id": uuid}}, false},
		{"act without notify user", map[string]any{"incident_response": map[string]any{"mode": "act"}}, true},
		{"act with bad notify user", map[string]any{"incident_response": map[string]any{"mode": "act", "notify_user_id": "nope"}}, true},
		{"bad project id", map[string]any{"incident_response": map[string]any{"mode": "shadow", "project_id": "nope"}}, true},
		{"unknown mode", map[string]any{"incident_response": map[string]any{"mode": "loud"}}, true},
		{"not an object value", map[string]any{"incident_response": "act"}, true},
	}
	for _, c := range cases {
		if err := validateIncidentResponseSetting(c.value); (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", c.name, err, c.wantErr)
		}
	}
}

// The member has no active DM when the incident opens: the incident is kept, the DM is held and
// delivered once by the first sweep after a DM exists.
func TestSweepIncidentsRetriesDMUntilDelivered(t *testing.T) {
	agentID := incidentSweepFixtureDM(t, "act", false)
	for sweep := 1; sweep <= 2; sweep++ {
		if err := testHandler.SweepIncidents(context.Background()); err != nil {
			t.Fatalf("sweep %d without DM: %v", sweep, err)
		}
		if issues, dms := incidentCounts(t); issues != 1 || dms != 0 {
			t.Fatalf("sweep %d without DM: %d incident issues and %d DMs, want 1 and 0", sweep, issues, dms)
		}
	}
	seedIncidentDM(t, agentID)
	for sweep := 3; sweep <= 4; sweep++ {
		if err := testHandler.SweepIncidents(context.Background()); err != nil {
			t.Fatalf("sweep %d with DM: %v", sweep, err)
		}
		if issues, dms := incidentCounts(t); issues != 1 || dms != 1 {
			t.Fatalf("sweep %d with DM: %d incident issues and %d DMs, want 1 and 1", sweep, issues, dms)
		}
	}
}
