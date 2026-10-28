package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Spec case 16 (docs/engineering/workflow-webhook-guard.md): cleanup deletes
// nonces older than 900 s and keeps newer ones.
func TestWebhookNonceSweepKeepsTheReplayWindow(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ctx := context.Background()
	q := db.New(testPool)
	ep, err := q.CreateWorkflowWebhookEndpoint(ctx, db.CreateWorkflowWebhookEndpointParams{
		WorkspaceID: parseUUID(testWorkspaceID), WorkflowID: parseUUID(uuid.NewString()), Name: "sweep",
		SecretSealed: []byte("sealed"), RatePerMin: 60, CreatedBy: parseUUID(testUserID),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM workflow_webhook_nonce WHERE endpoint_id=$1`, ep.ID)
		testPool.Exec(ctx, `DELETE FROM workflow_webhook_endpoint WHERE id=$1`, ep.ID)
	})
	for nonce, age := range map[string]string{"old": "1200 seconds", "edge-new": "600 seconds", "fresh": "0 seconds"} {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO workflow_webhook_nonce (endpoint_id, nonce, seen_at) VALUES ($1, $2, now() - $3::interval)`, ep.ID, nonce, age); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := sweepWebhookNonces(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	rows, err := testPool.Query(ctx, `SELECT nonce FROM workflow_webhook_nonce WHERE endpoint_id=$1 ORDER BY nonce`, ep.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		left = append(left, n)
	}
	if removed < 1 || len(left) != 2 || left[0] != "edge-new" || left[1] != "fresh" {
		t.Fatalf("removed=%d left=%v, want the 1200 s nonce gone and the other two kept", removed, left)
	}
}
