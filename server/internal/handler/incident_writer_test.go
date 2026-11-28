package handler

import (
	"context"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
)

// cleanIncidents removes the incident issues a test opened.
func cleanIncidents(t *testing.T, stoppedIssueID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM comment WHERE issue_id IN (SELECT incident_issue_id FROM incident_cause WHERE issue_id=$1)`, stoppedIssueID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id IN (SELECT incident_issue_id FROM incident_cause WHERE issue_id=$1)`, stoppedIssueID)
		testPool.Exec(ctx, `DELETE FROM incident_cause WHERE issue_id=$1`, stoppedIssueID)
	})
}

func incidentIssueFor(t *testing.T, stoppedIssueID, kind string) (id, title, body string, open bool) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT i.id, i.title, COALESCE(i.description,''), c.closed_at IS NULL FROM incident_cause c JOIN issue i ON i.id=c.incident_issue_id
		WHERE c.issue_id=$1 AND c.kind=$2 ORDER BY c.opened_at DESC LIMIT 1`, stoppedIssueID, kind).Scan(&id, &title, &body, &open)
	return
}

func incidentComments(t *testing.T, incidentID string) []string {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT content FROM comment WHERE issue_id=$1 ORDER BY created_at, id`, incidentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestIncidentWriterOpensAppendsAndCloses(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	stoppedID := dbfx.Issue(t, "stopped issue", map[string]any{"requested_by_id": testUserID, "number": 7000001})
	cleanIncidents(t, stoppedID)
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(stoppedID))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 19, 2, 0, 0, 0, time.UTC) // 09:00 ICT
	row := IncidentRow{Kind: "run_budget", Area: "run-budget", Impact: "run refused: used 15 of 15 runs",
		Row: "refused again", EventKey: "w1", At: start}

	// A new cause key opens an incident with impact, ICT start, source link and owner.
	if err := testHandler.RecordIncident(ctx, issue, row); err != nil {
		t.Fatal(err)
	}
	id, title, body, open := incidentIssueFor(t, stoppedID, "run_budget")
	if !strings.HasPrefix(title, "incident(run-budget): ") || !open {
		t.Fatalf("title = %q open = %v", title, open)
	}
	for _, want := range []string{"used 15 of 15 runs", "2026-11-19 09:00 ICT", "mention://issue/" + stoppedID} {
		if !strings.Contains(body, want) {
			t.Errorf("incident body lacks %q:\n%s", want, body)
		}
	}

	if n := dbfx.Count(t, `SELECT count(*) FROM issue WHERE id=$1 AND assignee_type='member' AND assignee_id=$2`, id, testUserID); n != 1 {
		t.Errorf("incident owner is not the requester")
	}

	// The same cause key appends a row and opens no second incident; the same event key twice appends once.
	row.EventKey, row.At = "w2", start.Add(30*time.Minute)
	for i := 0; i < 2; i++ {
		if err := testHandler.RecordIncident(ctx, issue, row); err != nil {
			t.Fatal(err)
		}
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM incident_cause WHERE issue_id=$1 AND kind='run_budget'`, stoppedID); n != 1 {
		t.Fatalf("incident_cause rows = %d, want 1", n)
	}
	if cs := incidentComments(t, id); len(cs) != 1 || !strings.Contains(cs[0], "09:30 ICT") || !strings.Contains(cs[0], "refused again") {
		t.Fatalf("timeline = %q, want one row at 09:30 ICT", cs)
	}

	// A different kind on the same issue is another cause key.
	other := row
	other.Kind, other.Area, other.EventKey = "blocked", "blocked", "x"
	if err := testHandler.RecordIncident(ctx, issue, other); err != nil {
		t.Fatal(err)
	}
	if otherID, _, _, _ := incidentIssueFor(t, stoppedID, "blocked"); otherID == id {
		t.Fatal("a different kind reused the incident")
	}

	// Leaving the stop state closes the row with the stop duration.
	if err := testHandler.CloseIncident(ctx, issue, "run_budget", start.Add(2*time.Hour+5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, _, _, open = incidentIssueFor(t, stoppedID, "run_budget")
	cs := incidentComments(t, id)
	if open || len(cs) != 2 || !strings.Contains(cs[1], "2 h 5 min") || !strings.Contains(cs[1], "11:05 ICT") {
		t.Fatalf("open = %v timeline = %q, want closed with 2 h 5 min at 11:05 ICT", open, cs)
	}

	// Closing again changes nothing; a later stop opens a new incident.
	if err := testHandler.CloseIncident(ctx, issue, "run_budget", start.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := incidentComments(t, id); len(got) != 2 {
		t.Fatalf("second close appended a row: %q", got)
	}
	row.EventKey = "w3"
	if err := testHandler.RecordIncident(ctx, issue, row); err != nil {
		t.Fatal(err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM incident_cause WHERE issue_id=$1 AND kind='run_budget'`, stoppedID); n != 2 {
		t.Fatalf("incident_cause rows after a new stop = %d, want 2", n)
	}
}

// Signal 1: a run refused on budget opens the incident.
func TestRunBudgetRefusalOpensIncident(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2310099, mergeEdgeSteps, 2)
	cleanIncidents(t, c.issueID)
	c.spend(t, 2)
	c.refuse(t)
	c.waitNotices(t, service.RunBudgetKindExhausted, 1)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && dbfx.Count(t, `SELECT count(*) FROM incident_cause WHERE issue_id=$1`, c.issueID) == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	_, title, body, open := incidentIssueFor(t, c.issueID, "run_budget")
	if !strings.HasPrefix(title, "incident(run-budget): ") || !open {
		t.Fatalf("title = %q open = %v", title, open)
	}
	if !strings.Contains(body, "used 2 of 2 runs") || !strings.Contains(body, "mention://issue/"+c.issueID) {
		t.Fatalf("incident body lacks the refusal or the source link:\n%s", body)
	}
}

// Leaving the stop state closes the budget incident with the duration.
func TestStatusMoveClosesBudgetIncident(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	stoppedID := dbfx.Issue(t, "stopped issue", map[string]any{"requested_by_id": testUserID, "number": 7000002})
	cleanIncidents(t, stoppedID)
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(stoppedID))
	if err != nil {
		t.Fatal(err)
	}
	row := IncidentRow{Kind: "run_budget", Area: "run-budget", Impact: "run refused: used 2 of 2 runs", Row: "r", EventKey: "w", At: time.Now().Add(-90 * time.Minute)}
	if err := testHandler.RecordIncident(ctx, issue, row); err != nil {
		t.Fatal(err)
	}
	publish := func(status string) {
		testHandler.Bus.Publish(events.Event{Type: protocol.EventIssueUpdated, WorkspaceID: uuidToString(issue.WorkspaceID),
			Payload: map[string]any{"issue": IssueResponse{ID: stoppedID, Status: status}, "status_changed": true, "activity_id": "test"}})
	}
	publish("blocked") // the move into the stop closes nothing
	id, _, _, open := incidentIssueFor(t, stoppedID, "run_budget")
	if !open {
		t.Fatal("the move into blocked closed the incident")
	}
	publish("in_progress")
	_, _, _, open = incidentIssueFor(t, stoppedID, "run_budget")
	if cs := incidentComments(t, id); open || len(cs) != 1 || !strings.Contains(cs[0], "1 h 30 min") {
		t.Fatalf("open = %v timeline = %q, want closed after 1 h 30 min", open, cs)
	}
}
