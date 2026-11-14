package handler

import (
	"context"
	"log"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// keepRuntimeFresh refreshes the fixture runtime's last_seen_at every `every`,
// like a daemon heartbeat. ClaimAgentTask only claims for a runtime seen within
// service.RuntimeClaimFreshnessSeconds, and the fixture stamps it once in
// TestMain, so a handler shard that runs longer than that window (-race, a
// loaded runner) stops claiming. It returns a stop func.
func keepRuntimeFresh(ctx context.Context, pool *pgxpool.Pool, runtimeID string, every time.Duration) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := pool.Exec(ctx, `UPDATE agent_runtime SET last_seen_at = now() WHERE id = $1`, runtimeID); err != nil && ctx.Err() == nil {
					log.Printf("keepRuntimeFresh: %v", err)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func TestFixtureRuntimeStaysClaimablePastFreshnessWindow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	dbfx.Exec(t, `UPDATE agent_runtime SET last_seen_at = now() - interval '200 seconds' WHERE id = $1`, testRuntimeID)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE agent_runtime SET last_seen_at = now() WHERE id = $1`, testRuntimeID)
	})
	stop := keepRuntimeFresh(context.Background(), testPool, testRuntimeID, 10*time.Millisecond)
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	for dbfx.Count(t, `SELECT count(*) FROM agent_runtime WHERE id = $1 AND last_seen_at > now() - interval '60 seconds'`, testRuntimeID) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("heartbeat did not refresh last_seen_at")
		}
		time.Sleep(10 * time.Millisecond)
	}

	agentID := createHandlerTestAgent(t, "Heartbeat claim agent", nil)
	issueID := dbfx.Issue(t, "heartbeat claim", testutil.Cols{"priority": "medium", "number": 88011})
	dbfx.Task(t, agentID, testutil.Cols{"runtime_id": testRuntimeID, "issue_id": issueID})

	req := newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+testRuntimeID+"/claim", nil)
	req = withURLParam(req, "runtimeId", testRuntimeID)
	w := testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK)

	var resp struct {
		Task *struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	w.JSON(&resp)
	if resp.Task == nil {
		t.Fatal("expected task in response: runtime last_seen_at went stale during the run")
	}
}
