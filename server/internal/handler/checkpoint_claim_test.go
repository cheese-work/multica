package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// TestClaimTaskByRuntime_CheckpointBlock_EndToEnd is the CHE-593 e2e
// integration test: claim a task, assert the rendered checkpoint block
// reaches the per-turn context (task.checkpoint_block, the MUL-5377 seam),
// and that an unresolved obligation recorded in a prior checkpoint survives
// into the next claim's rendered block untouched.
func TestClaimTaskByRuntime_CheckpointBlock_EndToEnd(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Checkpoint e2e claim runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Checkpoint e2e claim agent")

	// First claim: no prior checkpoint exists (cold start). CheckpointBlock
	// must come back empty — nothing to render yet — but the claim must
	// have persisted a first checkpoint row keyed on (issue_id, agent_id),
	// which the second claim below depends on.
	task1ID := createDispatchedClaimFixtureTask(t, ctx, agentID, runtimeID, issueID, "120 seconds", false)
	req1 := newDaemonTokenRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil,
		testWorkspaceID, "checkpoint-e2e-claim-1")
	req1 = withURLParam(req1, "runtimeId", runtimeID)
	w1 := testutil.Call(t, testHandler.ClaimTaskByRuntime, req1).Want(http.StatusOK)

	var resp1 struct {
		Task *struct {
			ID              string `json:"id"`
			CheckpointBlock string `json:"checkpoint_block"`
		} `json:"task"`
	}
	w1.JSON(&resp1)
	if resp1.Task == nil || resp1.Task.ID != task1ID {
		t.Fatalf("expected task %s claimed, got %+v (body=%s)", task1ID, resp1.Task, w1.Body.String())
	}
	if resp1.Task.CheckpointBlock != "" {
		t.Errorf("cold start: expected empty checkpoint_block, got %q", resp1.Task.CheckpointBlock)
	}

	// Move the claimed task out of the pending-slot window (queued/dispatched)
	// so the second claim's INSERT below doesn't collide with
	// idx_one_pending_task_per_issue_agent_thread (migration 452) — same
	// pattern as TestClaimTaskByRuntime_PopulatesWorkspaceContext's neighbors.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, task1ID)

	var checkpointCount int
	dbfx.QueryRow(t, `SELECT count(*) FROM issue_checkpoint WHERE issue_id = $1 AND agent_id = $2`,
		issueID, agentID).Scan(&checkpointCount)
	if checkpointCount != 1 {
		t.Fatalf("expected exactly one checkpoint row persisted after first claim, got %d", checkpointCount)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM issue_checkpoint WHERE issue_id = $1 AND agent_id = $2`, issueID, agentID)
	})

	// Seed the persisted checkpoint with an unresolved obligation and a
	// blocker, simulating what a real run would have recorded after this
	// first claim (this claim-time handler only maintains the coverage
	// cursor; content-contract fields are written by the run itself via the
	// same upsert path). This proves obligations survive claim-to-claim,
	// not just within a single Render call.
	dbfx.Exec(t, `
		UPDATE issue_checkpoint
		SET obligations = $1::jsonb, blockers = $2::jsonb, evidence = $3::jsonb
		WHERE issue_id = $4 AND agent_id = $5
	`,
		`[{"Description":"do not merge until human approves","Kind":"approval_restriction","SourceThreadID":"t1"}]`,
		`["waiting on CI"]`,
		`[{"Description":"native CI run","Ref":"https://ci.example/run/1","Identity":"github-actions"}]`,
		issueID, agentID,
	)

	// A new comment lands on the issue before the second claim — this is
	// the "changed thread" the checkpoint must flag for expansion, proving
	// the diff-against-live-scan step actually runs at claim time rather
	// than just replaying stored state.
	dbfx.Comment(t, issueID, "new feedback after the first claim")

	task2ID := createDispatchedClaimFixtureTask(t, ctx, agentID, runtimeID, issueID, "120 seconds", false)
	req2 := newDaemonTokenRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil,
		testWorkspaceID, "checkpoint-e2e-claim-2")
	req2 = withURLParam(req2, "runtimeId", runtimeID)
	w2 := testutil.Call(t, testHandler.ClaimTaskByRuntime, req2).Want(http.StatusOK)

	var resp2 struct {
		Task *struct {
			ID              string `json:"id"`
			CheckpointBlock string `json:"checkpoint_block"`
		} `json:"task"`
	}
	w2.JSON(&resp2)
	if resp2.Task == nil || resp2.Task.ID != task2ID {
		t.Fatalf("expected task %s claimed, got %+v (body=%s)", task2ID, resp2.Task, w2.Body.String())
	}

	block := resp2.Task.CheckpointBlock
	if block == "" {
		t.Fatal("expected non-empty checkpoint_block on second claim: a prior checkpoint exists")
	}
	if !containsAll(block,
		"do not merge until human approves",
		"[approval restriction]",
		"waiting on CI",
		"native CI run",
	) {
		t.Fatalf("expected obligation/blocker/evidence to survive into the rendered block, got:\n%s", block)
	}
	if !containsAll(block, "Changed or uncovered since this checkpoint") {
		t.Fatalf("expected the new comment's thread to be flagged as changed, got:\n%s", block)
	}

	var checkpointCountAfter int
	dbfx.QueryRow(t, `SELECT count(*) FROM issue_checkpoint WHERE issue_id = $1 AND agent_id = $2`,
		issueID, agentID).Scan(&checkpointCountAfter)
	if checkpointCountAfter != 1 {
		t.Fatalf("expected the second claim to upsert (not duplicate) the checkpoint row, got %d rows", checkpointCountAfter)
	}
}

// TestClaimTaskByRuntime_CheckpointDecodeFailure_DoesNotFailClaim covers the
// decode-failure branch the CHE-593 review flagged as untested: store_test.go
// proves FromRow errors on malformed JSON, but nothing proved
// loadIssueCheckpointBlock actually swallows that error rather than blanking
// the claim. This seeds a persisted checkpoint row with malformed
// obligations JSON directly (bypassing the handler, which never writes
// invalid JSON itself — this simulates a hand-edited or cross-version row)
// and asserts the claim still succeeds and degrades to a fresh checkpoint
// rather than failing.
func TestClaimTaskByRuntime_CheckpointDecodeFailure_DoesNotFailClaim(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Checkpoint decode-failure claim runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Checkpoint decode-failure claim agent")

	// "not an array" is valid JSON (a bare JSON string), so Postgres accepts
	// it into the jsonb column; it is invalid input for []Obligation, so
	// json.Unmarshal fails when FromRow decodes it — the failure mode this
	// test targets.
	dbfx.Exec(t, `
		INSERT INTO issue_checkpoint (workspace_id, issue_id, agent_id, issue_revision, candidate_id, obligations)
		VALUES ($1, $2, $3, 1, '', '"not an array"'::jsonb)
	`, testWorkspaceID, issueID, agentID)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM issue_checkpoint WHERE issue_id = $1 AND agent_id = $2`, issueID, agentID)
	})

	taskID := createDispatchedClaimFixtureTask(t, ctx, agentID, runtimeID, issueID, "120 seconds", false)
	req := newDaemonTokenRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil,
		testWorkspaceID, "checkpoint-decode-failure-claim")
	req = withURLParam(req, "runtimeId", runtimeID)
	w := testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK)

	var resp struct {
		Task *struct {
			ID              string `json:"id"`
			CheckpointBlock string `json:"checkpoint_block"`
		} `json:"task"`
	}
	w.JSON(&resp)
	if resp.Task == nil || resp.Task.ID != taskID {
		t.Fatalf("expected claim to succeed despite a malformed prior checkpoint row, got %+v (body=%s)", resp.Task, w.Body.String())
	}
	if resp.Task.CheckpointBlock != "" {
		t.Errorf("expected a malformed prior checkpoint to degrade to cold start (empty block), got %q", resp.Task.CheckpointBlock)
	}

	// The claim must still refresh the coverage cursor from live state even
	// though the prior row was unusable — cold start, not a stuck row.
	var obligations string
	dbfx.QueryRow(t, `SELECT obligations::text FROM issue_checkpoint WHERE issue_id = $1 AND agent_id = $2`,
		issueID, agentID).Scan(&obligations)
	if obligations != "[]" {
		t.Errorf("expected the refreshed checkpoint to reset obligations to [] on decode failure, got %q", obligations)
	}
}

func containsAll(haystack string, needles ...string) bool {
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			return false
		}
	}
	return true
}
