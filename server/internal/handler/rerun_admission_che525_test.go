package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

// TestRerunIssue_DuplicatePendingTaskCoalescesWithoutLeakingRawError is the C3
// acceptance test: when the CLI's authorized-force rerun (POST
// /api/issues/{id}/rerun, zero-arg assignee path) loses every reclaim attempt
// to a relentless concurrent competitor for the same (issue, agent) pending
// slot, the handler must classify the resulting service.ErrDuplicatePendingTask
// the same way C1 (issue_trigger.go) and C2 (comment.go) already classify it at
// their own admission boundaries: a benign coalesce, never a raw constraint
// name or an opaque 400 indistinguishable from a real failure.
//
// The zero-arg assignee-rerun path is the one that exercises this: it carries
// no sourceTaskID, so TaskService.RerunIssue's lineage reconciliation
// (findLiveRerun) never fires for it, and an exhausted reclaim loop returns the
// duplicate error to the handler unreconciled — exactly the shape a genuine
// "something is enqueueing on this slot in a tight loop" contention produces.
func TestRerunIssue_DuplicatePendingTaskCoalescesWithoutLeakingRawError(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "CHE-525 rerun admission runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "CHE-525 rerun admission agent")
	if _, err := testPool.Exec(ctx, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, issueID, agentID); err != nil {
		t.Fatalf("assign issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})

	insertCompetingPendingTask := func() {
		testPool.Exec(ctx, `
			INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
			VALUES ($1, $2, $3, 'queued', 0)
			ON CONFLICT DO NOTHING
		`, agentID, runtimeID, issueID)
	}
	insertCompetingPendingTask()

	// Keep re-occupying the pending slot faster than RerunIssue's bounded
	// reclaim loop (maxRerunAttempts) can win it, so every attempt inside this
	// single handler call collides — the deterministic stand-in for "something
	// enqueueing in a loop" that the service comment calls the genuinely
	// pathological case. Many tight-loop writers (no sleep) maximize the odds
	// the slot is re-occupied the instant a reclaim's cancel commits.
	const competingWriters = 12
	stop := make(chan struct{})
	var contending atomic.Bool
	contending.Store(true)
	var wg sync.WaitGroup
	for i := 0; i < competingWriters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if contending.Load() {
					insertCompetingPendingTask()
				}
			}
		}()
	}
	t.Cleanup(func() {
		close(stop)
		wg.Wait()
	})

	req := newRequest("POST", "/api/issues/"+issueID+"/rerun", map[string]any{"reason": "che-525 forced admission test"})
	req = withURLParam(req, "id", issueID)
	w := httptest.NewRecorder()
	testHandler.RerunIssue(w, req)
	contending.Store(false)

	switch w.Code {
	case http.StatusAccepted:
		// The reclaim loop won inside its attempt budget — a legitimate
		// outcome under this adversarial contention; the coalesce path this
		// test targets did not trigger on this run. Nothing to assert.
		t.Skip("reclaim loop won under contention; duplicate-coalesce path not exercised this run")
	case http.StatusConflict:
		var body map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode conflict body: %v (body=%s)", err, w.Body.String())
		}
		if strings.Contains(strings.ToLower(body["error"]), "idx_one_pending_task") ||
			strings.Contains(strings.ToLower(body["error"]), "constraint") {
			t.Errorf("duplicate-coalesce response leaked raw constraint detail: %q", body["error"])
		}
	default:
		t.Fatalf("RerunIssue under slot contention: expected 202 or 409, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRerunIssueRequest_ReasonIsNotPersisted proves the CLI's optional
// --reason (RerunIssueRequest.Reason) is accepted and does not change
// admission behavior or get written onto the task row — it exists purely for
// the audit log line, not as task state.
func TestRerunIssueRequest_ReasonIsNotPersisted(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "CHE-525 reason runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "CHE-525 reason agent")
	if _, err := testPool.Exec(ctx, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, issueID, agentID); err != nil {
		t.Fatalf("assign issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})

	req := newRequest("POST", "/api/issues/"+issueID+"/rerun", map[string]any{"reason": "authorized force: prior run misclassified"})
	req = withURLParam(req, "id", issueID)
	w := httptest.NewRecorder()
	testHandler.RerunIssue(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("RerunIssue: expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	var row struct {
		Context []byte
	}
	if err := testPool.QueryRow(ctx, `SELECT context FROM agent_task_queue WHERE id = $1`, util.MustParseUUID(resp.ID)).
		Scan(&row.Context); err != nil {
		t.Fatalf("read task context: %v", err)
	}
	if strings.Contains(string(row.Context), "authorized force") {
		t.Errorf("reason text leaked into persisted task context: %s", string(row.Context))
	}
}
