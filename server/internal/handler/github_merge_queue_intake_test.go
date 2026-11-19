package handler

import (
	"context"
	"testing"
)

// B2 (CHE-7409): the dequeued webhook stores a pending intake row with the
// workspace's routing at that commit; enqueued, dequeued and closed move the
// PR's queue state.

type mqIntakeRow struct{ delivery, routing, status string }

func readMQIntake(t *testing.T, pr int) []mqIntakeRow {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT coalesce(delivery_id,''), routing, status FROM merge_queue_intake WHERE workspace_id = $1 AND pr_number = $2 ORDER BY created_at`, testWorkspaceID, pr)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []mqIntakeRow
	for rows.Next() {
		var r mqIntakeRow
		if err := rows.Scan(&r.delivery, &r.routing, &r.status); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func readMQState(t *testing.T, pr int) (state string, rows int) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM merge_queue_state WHERE workspace_id = $1 AND pr_number = $2`, testWorkspaceID, pr).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	_ = testPool.QueryRow(context.Background(), `SELECT state FROM merge_queue_state WHERE workspace_id = $1 AND pr_number = $2`, testWorkspaceID, pr).Scan(&state)
	return state, rows
}

func cleanMQ(t *testing.T) {
	t.Helper()
	var settings []byte
	if err := testPool.QueryRow(context.Background(), `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&settings); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM merge_queue_intake WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(context.Background(), `DELETE FROM merge_queue_state WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(context.Background(), `UPDATE workspace SET settings = $2 WHERE id = $1`, testWorkspaceID, settings)
	})
}

func TestDequeuedWebhookStoresOnePendingIntakeRowPerDelivery(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 740901
	fx := newPRMergeWebhookFixture(t, installationID)
	cleanMQ(t)
	body := buildDequeuedPRWebhookBody(fx.child.Identifier+": queued", 740911, installationID)

	postSignedGitHubWebhook(t, fx.secret, body, "guid-7409-a")
	postSignedGitHubWebhook(t, fx.secret, body, "guid-7409-a") // redelivery
	got := readMQIntake(t, 740911)
	if len(got) != 1 || got[0].delivery != "guid-7409-a" || got[0].routing != "warn" || got[0].status != "pending" {
		t.Fatalf("intake rows = %+v; want one pending row for guid-7409-a with the default routing warn", got)
	}
	if state, n := readMQState(t, 740911); n != 1 || state != "dequeued" {
		t.Errorf("queue state = %q (%d rows); want dequeued (1 row)", state, n)
	}
}

func TestDequeuedWebhookRoutingIsTheValueAtItsCommit(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 740902
	fx := newPRMergeWebhookFixture(t, installationID)
	cleanMQ(t)
	body := buildDequeuedPRWebhookBody(fx.child.Identifier+": queued", 740912, installationID)
	postSignedGitHubWebhook(t, fx.secret, body, "guid-7409-r1")

	// The owner turns routing off after the commit: that row keeps warn, and a
	// later dequeue stores nothing.
	if _, err := testPool.Exec(context.Background(), `UPDATE workspace SET settings = settings || '{"merge_queue_dequeue_routing":"off"}'::jsonb WHERE id = $1`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	body["pull_request"].(map[string]any)["updated_at"] = "2026-11-06T05:00:00Z"
	postSignedGitHubWebhook(t, fx.secret, body, "guid-7409-r2")
	got := readMQIntake(t, 740912)
	if len(got) != 1 || got[0].routing != "warn" {
		t.Fatalf("intake rows = %+v; want the first row only, still warn", got)
	}
}

func TestQueueWebhooksMoveTheQueueState(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 740903
	fx := newPRMergeWebhookFixture(t, installationID)
	cleanMQ(t)
	body := buildDequeuedPRWebhookBody(fx.child.Identifier+": queued", 740913, installationID)
	steps := []struct{ action, at, want string }{
		{"enqueued", "2026-11-06T04:00:00Z", "enqueued"},
		{"dequeued", "2026-11-06T04:10:00Z", "dequeued"},
		{"closed", "2026-11-06T04:20:00Z", "closed"},
	}
	for i, s := range steps {
		body["action"] = s.action
		body["pull_request"].(map[string]any)["updated_at"] = s.at
		postSignedGitHubWebhook(t, fx.secret, body, "guid-7409-s"+string(rune('a'+i)))
		if state, n := readMQState(t, 740913); state != s.want || n != 1 {
			t.Fatalf("after %s: state %q (%d rows); want %s (1 row)", s.action, state, n, s.want)
		}
	}
	if got := readMQIntake(t, 740913); len(got) != 1 {
		t.Errorf("intake rows = %d; want 1, from the dequeued delivery only", len(got))
	}
}
