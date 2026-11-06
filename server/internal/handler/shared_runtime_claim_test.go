package handler

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// sharedClaimAgent returns the agent and runtime seeded once in TestMain and
// marks that runtime freshly seen. The seed heartbeat is written once per
// package run, and a shard that runs longer than
// service.RuntimeClaimFreshnessSeconds would otherwise make every claim on the
// shared runtime return no task, depending on which earlier tests happened to
// refresh it (CHE-4008).
func sharedClaimAgent(t *testing.T) (agentID, runtimeID string) {
	t.Helper()
	dbfx.QueryRow(t,
		`SELECT id, runtime_id FROM agent WHERE id = $1`,
		testSeededAgentID,
	).Scan(&agentID, &runtimeID)
	refreshRuntimeHeartbeat(t, runtimeID)
	return agentID, runtimeID
}

// refreshRuntimeHeartbeat stands in for the daemon heartbeat a long-lived
// fixture runtime never receives, so the claim freshness fence admits it.
func refreshRuntimeHeartbeat(t *testing.T, runtimeID string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE agent_runtime SET last_seen_at = now() WHERE id = $1`, runtimeID)
}

// A package run longer than service.RuntimeClaimFreshnessSeconds ages the
// runtime seeded in TestMain past the claim freshness fence (CHE-4008).
func TestSharedClaimAgentClaimableAfterLongPackageRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	dbfx.Exec(t, `UPDATE agent_runtime SET last_seen_at = now() - interval '10 minutes' WHERE id = $1`, testRuntimeID)

	agentID, runtimeID := sharedClaimAgent(t)
	issueID := dbfx.Issue(t, "shared runtime claim", testutil.Cols{"priority": "medium"})
	dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID,
		"issue_id":   issueID,
	})

	if task, body := claimTaskByRuntimeForTest(t, runtimeID); task == nil {
		t.Fatalf("expected task in response, got %s", body)
	}
}
