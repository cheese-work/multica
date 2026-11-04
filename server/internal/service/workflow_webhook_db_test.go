package service

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Queries behind the workflow webhook admission transaction (PR #425 spec).
// The properties below only exist in SQL: single-use nonces, ref dedupe and
// endpoint-keyed guard lookup. There are no foreign keys, so ids are random.

func randUUID(t *testing.T) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if _, err := rand.Read(u.Bytes[:]); err != nil {
		t.Fatal(err)
	}
	u.Valid = true
	return u
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func newWebhookEndpoint(t *testing.T, q *db.Queries, workflowID pgtype.UUID, name string) db.WorkflowWebhookEndpoint {
	t.Helper()
	ctx := context.Background()
	ws := randUUID(t)
	ep, err := q.CreateWorkflowWebhookEndpoint(ctx, db.CreateWorkflowWebhookEndpointParams{
		WorkspaceID: ws, WorkflowID: workflowID, Name: name, SecretSealed: []byte("sealed-1"), RatePerMin: 60,
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	t.Cleanup(func() {
		pool := sharedTestPool(t)
		pool.Exec(context.Background(), `DELETE FROM workflow_webhook_endpoint WHERE id = $1`, ep.ID)
		pool.Exec(context.Background(), `DELETE FROM workflow_webhook_nonce WHERE endpoint_id = $1`, ep.ID)
		pool.Exec(context.Background(), `DELETE FROM workflow_webhook_event WHERE endpoint_id = $1`, ep.ID)
	})
	return ep
}

func TestWebhookNonceIsSingleUse(t *testing.T) {
	q := db.New(sharedTestPool(t))
	ctx := context.Background()
	ep := newWebhookEndpoint(t, q, randUUID(t), "ci")

	arg := db.InsertWorkflowWebhookNonceParams{EndpointID: ep.ID, Nonce: "abcdefghijklmnop"}
	if n, err := q.InsertWorkflowWebhookNonce(ctx, arg); err != nil || n != 1 {
		t.Fatalf("first insert: rows=%d err=%v", n, err)
	}
	if n, err := q.InsertWorkflowWebhookNonce(ctx, arg); err != nil || n != 0 {
		t.Fatalf("replay must insert 0 rows without error: rows=%d err=%v", n, err)
	}
	other := newWebhookEndpoint(t, q, randUUID(t), "ci")
	arg.EndpointID = other.ID
	if n, err := q.InsertWorkflowWebhookNonce(ctx, arg); err != nil || n != 1 {
		t.Fatalf("same nonce on another endpoint is not a replay: rows=%d err=%v", n, err)
	}
}

func TestWebhookNonceWindowAndCleanup(t *testing.T) {
	q := db.New(sharedTestPool(t))
	ctx := context.Background()
	ep := newWebhookEndpoint(t, q, randUUID(t), "ci")
	for _, n := range []string{"nonce-aaaaaaaaaaaa", "nonce-bbbbbbbbbbbb", "nonce-cccccccccccc"} {
		if _, err := q.InsertWorkflowWebhookNonce(ctx, db.InsertWorkflowWebhookNonceParams{EndpointID: ep.ID, Nonce: n}); err != nil {
			t.Fatal(err)
		}
	}
	count := func(since time.Time) int64 {
		n, err := q.CountWorkflowWebhookNoncesSince(ctx, db.CountWorkflowWebhookNoncesSinceParams{EndpointID: ep.ID, SeenAt: ts(since)})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count(time.Now().Add(-time.Minute)); got != 3 {
		t.Fatalf("count in window = %d, want 3", got)
	}
	if got := count(time.Now().Add(time.Minute)); got != 0 {
		t.Fatalf("count after window = %d, want 0", got)
	}
	if n, _ := q.DeleteExpiredWorkflowWebhookNonces(ctx, ts(time.Now().Add(-15*time.Minute))); n != 0 {
		t.Fatalf("cleanup deleted %d fresh rows", n)
	}
	if n, _ := q.DeleteExpiredWorkflowWebhookNonces(ctx, ts(time.Now().Add(time.Minute))); n < 3 {
		t.Fatalf("cleanup deleted %d rows, want at least 3", n)
	}
}

func webhookEvent(ep db.WorkflowWebhookEndpoint, issue pgtype.UUID, result, ref string) db.InsertWorkflowWebhookEventParams {
	p := db.InsertWorkflowWebhookEventParams{
		EndpointID: ep.ID, WorkspaceID: ep.WorkspaceID, IssueID: issue, Name: ep.Name,
		Result: result, BodySha256: make([]byte, 32), BodyBytes: 40,
	}
	if ref != "" {
		p.Ref = pgtype.Text{String: ref, Valid: true}
	}
	return p
}

func TestWebhookEventDedupesOnRef(t *testing.T) {
	q := db.New(sharedTestPool(t))
	ctx := context.Background()
	ep := newWebhookEndpoint(t, q, randUUID(t), "ci")
	issue := randUUID(t)

	if n, err := q.InsertWorkflowWebhookEvent(ctx, webhookEvent(ep, issue, "success", "run-1")); err != nil || n != 1 {
		t.Fatalf("first: rows=%d err=%v", n, err)
	}
	if n, err := q.InsertWorkflowWebhookEvent(ctx, webhookEvent(ep, issue, "success", "run-1")); err != nil || n != 0 {
		t.Fatalf("same ref must be a no-op: rows=%d err=%v", n, err)
	}
	if n, err := q.InsertWorkflowWebhookEvent(ctx, webhookEvent(ep, issue, "success", "run-2")); err != nil || n != 1 {
		t.Fatalf("new ref: rows=%d err=%v", n, err)
	}
	for i := 0; i < 2; i++ {
		if n, err := q.InsertWorkflowWebhookEvent(ctx, webhookEvent(ep, issue, "success", "")); err != nil || n != 1 {
			t.Fatalf("events without ref are never deduped: rows=%d err=%v", n, err)
		}
	}
}

func TestWebhookGuardLookupIsEndpointScopedSuccessOnlyAndAfterEntry(t *testing.T) {
	q := db.New(sharedTestPool(t))
	ctx := context.Background()
	epA := newWebhookEndpoint(t, q, randUUID(t), "ci")
	epB := newWebhookEndpoint(t, q, randUUID(t), "ci") // same name, other workflow
	issue := randUUID(t)
	lookup := func(ep db.WorkflowWebhookEndpoint, entered time.Time) error {
		_, err := q.GetLatestWorkflowWebhookSuccess(ctx, db.GetLatestWorkflowWebhookSuccessParams{
			EndpointID: ep.ID, IssueID: issue, ReceivedAt: ts(entered),
		})
		return err
	}

	if _, err := q.InsertWorkflowWebhookEvent(ctx, webhookEvent(epA, issue, "failure", "")); err != nil {
		t.Fatal(err)
	}
	if lookup(epA, time.Now().Add(-time.Minute)) == nil {
		t.Fatal("a failure event must not satisfy the guard")
	}
	if _, err := q.InsertWorkflowWebhookEvent(ctx, webhookEvent(epA, issue, "success", "")); err != nil {
		t.Fatal(err)
	}
	if err := lookup(epA, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("success after step entry must satisfy the guard: %v", err)
	}
	if lookup(epA, time.Now().Add(time.Minute)) == nil {
		t.Fatal("an event received before step entry must not satisfy the guard")
	}
	if lookup(epB, time.Now().Add(-time.Minute)) == nil {
		t.Fatal("another endpoint with the same name must not satisfy the guard")
	}
}

func TestWebhookEndpointNameUniquePerWorkflowAndRotation(t *testing.T) {
	q := db.New(sharedTestPool(t))
	ctx := context.Background()
	wf := randUUID(t)
	ep := newWebhookEndpoint(t, q, wf, "ci")

	if _, err := q.CreateWorkflowWebhookEndpoint(ctx, db.CreateWorkflowWebhookEndpointParams{
		WorkspaceID: ep.WorkspaceID, WorkflowID: wf, Name: "ci", SecretSealed: []byte("x"), RatePerMin: 60,
	}); err == nil {
		t.Fatal("duplicate (workflow, name) must be rejected")
	}

	exp := time.Now().Add(24 * time.Hour)
	rot, err := q.RotateWorkflowWebhookEndpointSecret(ctx, db.RotateWorkflowWebhookEndpointSecretParams{
		ID: ep.ID, SecretSealed: []byte("sealed-2"), PreviousExpiresAt: ts(exp),
	})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if string(rot.SecretSealed) != "sealed-2" || string(rot.PreviousSecretSealed) != "sealed-1" || !rot.PreviousExpiresAt.Valid {
		t.Fatalf("rotation must keep the old secret as previous: %+v", rot)
	}

	dis, err := q.DisableWorkflowWebhookEndpoint(ctx, ep.ID)
	if err != nil || !dis.DisabledAt.Valid || dis.PreviousSecretSealed != nil {
		t.Fatalf("disable must stop the endpoint and drop the previous secret: %+v err=%v", dis, err)
	}
	if _, err := q.RotateWorkflowWebhookEndpointSecret(ctx, db.RotateWorkflowWebhookEndpointSecretParams{
		ID: ep.ID, SecretSealed: []byte("sealed-3"), PreviousExpiresAt: ts(exp),
	}); err == nil {
		t.Fatal("a disabled endpoint must not rotate")
	}
}
