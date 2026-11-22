package handler

import (
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/browserlease"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func seedQueuedDecisionBoundBrowserLease(t *testing.T, taskID, decisionID string) string {
	t.Helper()
	id := uuid.NewString()
	dbfx.Exec(t, `INSERT INTO browser_lease (id, workspace_id, instance_id, profile_id, agent_id, task_id, issue_id, user_id, decision_id, class, state, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'S2', 'queued', now() + interval '2 hours')`,
		id, testWorkspaceID, uuid.NewString(), uuid.NewString(), uuid.NewString(), taskID, uuid.NewString(), testUserID, decisionID)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM browser_lease WHERE id = $1`, id) })
	return id
}

// One once-per-issue decision covers many leases. The human withdraws the answer through the
// real endpoint: every active and queued lease of that decision ends with one revoke event
// each, and a lease of another decision stays live (CHE-4782, W3-19).
func TestWithdrawingOneDecisionRevokesEveryActiveAndQueuedLeaseItCovers(t *testing.T) {
	fixture := newRecordedCardFixture(t)
	decision := uuidToString(fixture.decision.ID)
	runtimeID := dbfx.Runtime(t, "CHE-4782 runtime")
	agentID := dbfx.Agent(t, "CHE-4782 agent "+uuid.NewString(), runtimeID)
	issueID := dbfx.Issue(t, "CHE-4782 issue")
	task := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "started_at": testutil.Raw("now()")})

	covered := []string{
		seedDecisionBoundBrowserLease(t, task, decision),
		seedDecisionBoundBrowserLease(t, task, decision),
		seedQueuedDecisionBoundBrowserLease(t, task, decision),
	}
	elsewhere := seedDecisionBoundBrowserLease(t, task, uuid.NewString())

	var mu sync.Mutex
	revoked := map[string]string{}
	testHandler.Bus.Subscribe(protocol.EventBrowserLeaseRevoked, func(e events.Event) {
		payload := e.Payload.(map[string]any)
		mu.Lock()
		defer mu.Unlock()
		revoked[payload["lease_id"].(string)] = payload["reason"].(string)
	})

	fixture.disclaim(t)

	for _, id := range covered {
		var state string
		err := testPool.QueryRow(t.Context(), `SELECT state FROM browser_lease WHERE id = $1`, id).Scan(&state)
		// A queued lease is deleted (no row); an active one is ended.
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			t.Fatalf("read lease %s: %v", id, err)
		case state != "ended":
			t.Fatalf("lease %s still %s after the decision was withdrawn", id, state)
		}
		mu.Lock()
		reason, ok := revoked[id]
		mu.Unlock()
		if !ok || reason != browserlease.ReasonDecisionWithdrawn {
			t.Fatalf("lease %s: revoke event reason %q (seen %v), want %s", id, reason, ok, browserlease.ReasonDecisionWithdrawn)
		}
	}
	if state, _ := browserLeaseState(t, elsewhere); state != "active" {
		t.Fatalf("a lease of another decision was revoked: state = %q", state)
	}
}
