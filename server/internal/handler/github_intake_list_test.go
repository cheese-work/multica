package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListGitHubIntakeShowsWorkspaceRowsAndHidesOthers(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	const prefix = "che2669-"
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM github_webhook_revision WHERE source_delivery_guid LIKE $1`, prefix+"%")
		testPool.Exec(ctx, `DELETE FROM github_webhook_intake WHERE delivery_guid LIKE $1`, prefix+"%")
	})
	for _, g := range []string{"mine", "unclaimed", "theirs"} {
		dbfx.Exec(t, `INSERT INTO github_webhook_intake (delivery_guid, event, signature, payload, state, reason) VALUES ($1, 'issue_comment', 'sig', '{}', 'projected', 'notify_only')`, prefix+g)
	}
	insertRevision := func(guid, workspace string) {
		dbfx.Exec(t, `INSERT INTO github_webhook_revision (workspace_id, installation_id, repository_id, pr_number, object_kind, object_id, body_hash, source_delivery_guid, event, action, sender_id, author_id, author_login, author_type, head_sha, review_state, state, reason)
			VALUES ($1, 1, 1, 1, 'issue_comment', $3, repeat('a', 64), $2, 'issue_comment', 'created', 1, 1, 'x', 'User', 'h', '', 'projected', 'notify_only')`, workspace, guid, len(guid))
	}
	insertRevision(prefix+"mine", testWorkspaceID)
	insertRevision(prefix+"theirs", "00000000-0000-4000-8000-000000002669")

	w := httptest.NewRecorder()
	req := newRequest(http.MethodGet, "/api/github/intake?since=2000-01-01T00:00:00Z&limit=500", nil)
	testHandler.ListGitHubIntake(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Deliveries []struct {
			DeliveryGUID  string `json:"delivery_guid"`
			RevisionState string `json:"revision_state"`
			Tasks         int    `json:"tasks"`
		} `json:"deliveries"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, d := range resp.Deliveries {
		seen[d.DeliveryGUID] = true
	}
	if !seen[prefix+"mine"] || !seen[prefix+"unclaimed"] || seen[prefix+"theirs"] {
		t.Fatalf("deliveries = %v, want mine+unclaimed and not theirs", seen)
	}

	bad := httptest.NewRecorder()
	testHandler.ListGitHubIntake(bad, newRequest(http.MethodGet, "/api/github/intake?since=yesterday", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad since status = %d, want 400", bad.Code)
	}
}
