package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// heldInputEvents returns the event ids of the issue's held inputs.
func heldInputEvents(t *testing.T, issueID string) []string {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT event_id FROM run_input WHERE issue_id = $1 AND state = 'owned' ORDER BY event_id`, issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ev string
		if err := rows.Scan(&ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

func queuedRunFor(t *testing.T, issueID, agentID, runtimeID string) {
	t.Helper()
	dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID})
	dbfx.Cleanup(t, `DELETE FROM run_input WHERE issue_id = $1`, issueID)
}

// A status change that a queued run absorbs reaches the run as an input under
// the event id the producer sent, so a redelivery adds nothing.
func TestStatusChangeAbsorbedByQueuedRunIsStoredWithProducerEvent(t *testing.T) {
	fixture := newTriageRunFixture(t)
	issueID := fixture.issueFor(t, "", "coalesced status")
	dbfx.Exec(t, `UPDATE issue SET status = 'backlog' WHERE id = $1`, issueID)
	queuedRunFor(t, issueID, fixture.agentID, fixture.runtimeID)

	send := func() *httptest.ResponseRecorder {
		req := withURLParam(newRequest(http.MethodPut, "/api/issues/"+issueID, map[string]any{"status": "todo"}), "id", issueID)
		req.Header.Set(EventIDHeader, "evt-status-run")
		rec := httptest.NewRecorder()
		testHandler.IdempotentWrite(http.HandlerFunc(testHandler.UpdateIssue)).ServeHTTP(rec, req)
		return rec
	}
	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("update %d: %s", rec.Code, rec.Body.String())
	}
	if got := heldInputEvents(t, issueID); len(got) != 1 || got[0] != "event:evt-status-run" {
		t.Fatalf("held inputs = %v, want [event:evt-status-run]", got)
	}
	var payload map[string]any
	var raw []byte
	if err := testPool.QueryRow(t.Context(), `SELECT payload FROM run_input WHERE issue_id = $1`, issueID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload["event_id"] != "evt-status-run" || payload["source"] != "status" {
		t.Fatalf("payload = %s, want the producer event id and source status", raw)
	}
	send()
	if got := heldInputEvents(t, issueID); len(got) != 1 {
		t.Fatalf("redelivery changed the inputs: %v", got)
	}
}

// The preview shares the predicate and must write nothing.
func TestTriggerPreviewDoesNotStoreInputs(t *testing.T) {
	fixture := newTriageRunFixture(t)
	issueID := fixture.issueFor(t, "", "preview status")
	dbfx.Exec(t, `UPDATE issue SET status = 'backlog' WHERE id = $1`, issueID)
	queuedRunFor(t, issueID, fixture.agentID, fixture.runtimeID)

	testutil.Call(t, testHandler.PreviewIssueTrigger,
		newRequest(http.MethodPost, "/api/issues/preview-trigger", map[string]any{"issue_ids": []string{issueID}, "status": "todo"})).Want(http.StatusOK)
	if got := heldInputEvents(t, issueID); len(got) != 0 {
		t.Fatalf("preview stored inputs: %v", got)
	}
}

func TestChildDoneAbsorbedByQueuedRunIsStored(t *testing.T) {
	fixture := newChildDoneFixture(t, "in_progress")
	agentID := handlerTestAgentID(t)
	setIssueAssigneeDirect(t, fixture.parent.ID, "agent", agentID)
	var runtimeID string
	if err := testPool.QueryRow(t.Context(), `SELECT runtime_id FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	queuedRunFor(t, fixture.parent.ID, agentID, runtimeID)

	parent, err := testHandler.Queries.GetIssue(t.Context(), parseUUID(fixture.parent.ID))
	if err != nil {
		t.Fatal(err)
	}
	testHandler.triggerChildDoneAgent(t.Context(), parent, pgtype.UUID{})
	got := heldInputEvents(t, fixture.parent.ID)
	if len(got) != 1 || got[0][:len("child_done:"+util.UUIDToString(parent.ID))] != "child_done:"+util.UUIDToString(parent.ID) {
		t.Fatalf("held inputs = %v, want one child_done input", got)
	}
}
