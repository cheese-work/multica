package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type intakeListRow struct {
	DeliveryGUID  string `json:"delivery_guid"`
	RevisionState string `json:"revision_state"`
	Tasks         int    `json:"tasks"`
}

func listGitHubIntakeForTest(t *testing.T, query string) []intakeListRow {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.ListGitHubIntake(w, newRequest(http.MethodGet, "/api/github/intake?"+query, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Deliveries []intakeListRow `json:"deliveries"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp.Deliveries
}

func TestListGitHubIntakeHidesOtherWorkspacesAndResolvesRedeliveries(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fixture := newGitHubIntakeFixture(t)
	const since = "since=2000-01-01T00:00:00Z&limit=500"
	for _, suffix := range []string{"first", "redelivery"} {
		if response := fixture.post("pull_request_review", fixture.secret, fixture.issueID+"-L-"+suffix); response.Code != http.StatusAccepted {
			t.Fatalf("intake: %d %s", response.Code, response.Body.String())
		}
	}
	// Rows with no workspace association (unknown installation, bad payload) stay hidden.
	for _, g := range []string{"badjson", "unclaimed"} {
		payload := "not json"
		if g == "unclaimed" {
			payload = `{"installation":{"id":1}}`
		}
		dbfx.Exec(t, `INSERT INTO github_webhook_intake (delivery_guid, event, signature, payload, state, reason) VALUES ($1, 'pull_request_review', 'sig', $2, 'denied', 'x')`, fixture.issueID+"-L-"+g, []byte(payload))
	}
	// A dispatch receipt with a task on the canonical revision counts for the redelivery too.
	dbfx.Exec(t, `INSERT INTO github_webhook_dispatch (workspace_id, object_kind, object_id, body_hash, issue_id, comment_id, task_id, revision_id, state, reason)
		SELECT workspace_id, object_kind, object_id, body_hash, $2::uuid, $2::uuid, $2::uuid, id, 'dispatched', 'admitted' FROM github_webhook_revision WHERE workspace_id=$1`, testWorkspaceID, fixture.issueID)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM github_webhook_dispatch WHERE workspace_id=$1`, testWorkspaceID)
	})

	seen := map[string]intakeListRow{}
	for _, d := range listGitHubIntakeForTest(t, since) {
		seen[d.DeliveryGUID] = d
	}
	if _, ok := seen[fixture.issueID+"-L-badjson"]; ok {
		t.Fatalf("bad payload row leaked: %v", seen)
	}
	if _, ok := seen[fixture.issueID+"-L-unclaimed"]; ok {
		t.Fatalf("unassociated row leaked: %v", seen)
	}
	for _, suffix := range []string{"first", "redelivery"} {
		row, ok := seen[fixture.issueID+"-L-"+suffix]
		if !ok || row.RevisionState == "" || row.Tasks != 1 {
			t.Fatalf("%s = %+v (found %v), want revision state and 1 task", suffix, row, ok)
		}
	}

	// The same revision under another workspace is invisible here.
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM github_webhook_revision WHERE workspace_id='00000000-0000-4000-8000-000000002669'`)
	})
	_, _ = testPool.Exec(context.Background(), `DELETE FROM github_webhook_revision WHERE workspace_id='00000000-0000-4000-8000-000000002669'`)
	dbfx.Exec(t, `UPDATE github_webhook_revision SET workspace_id='00000000-0000-4000-8000-000000002669' WHERE workspace_id=$1`, testWorkspaceID)
	for _, d := range listGitHubIntakeForTest(t, since) {
		if d.DeliveryGUID == fixture.issueID+"-L-first" || d.DeliveryGUID == fixture.issueID+"-L-redelivery" {
			t.Fatalf("other workspace's delivery leaked: %+v", d)
		}
	}

	w := httptest.NewRecorder()
	testHandler.ListGitHubIntake(w, newRequest(http.MethodGet, "/api/github/intake?since=yesterday", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad since status = %d, want 400", w.Code)
	}
}
