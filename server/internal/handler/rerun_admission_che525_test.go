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

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
)

// TestWriteRerunIssueError_DuplicatePendingTaskCoalescesWithoutLeakingRawError
// is the C3 acceptance test for the admission-boundary classification:
// writeRerunIssueError must treat service.ErrDuplicatePendingTask as the same
// benign coalesce outcome C1 (issue_trigger.go, CHE-486) and C2 (comment.go,
// CHE-524) already classify at their own admission boundaries — a 409 with a
// stable message, never a raw sentinel/constraint string, and never the
// warn-level treatment reserved for a genuine failure.
//
// This drives the classifier directly with the exact typed error
// TaskService.RerunIssue returns bare (unreconciled) when the zero-arg
// assignee-rerun path exhausts its bounded reclaim loop under sustained
// pending-slot contention (see TestRerunIssue_DuplicatePendingTaskCoalesces
// below for a real-contention probe of that path) — deterministic, no
// flakiness, and it cannot silently skip the assertion in CI.
func TestWriteRerunIssueError_DuplicatePendingTaskCoalescesWithoutLeakingRawError(t *testing.T) {
	w := httptest.NewRecorder()
	writeRerunIssueError(w, "issue-1", service.ErrDuplicatePendingTask)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusConflict)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode conflict body: %v (body=%s)", err, w.Body.String())
	}
	if body["error"] != "a rerun for this issue and agent is already in progress" {
		t.Errorf("error = %q, want the stable coalesce message", body["error"])
	}
	lower := strings.ToLower(body["error"])
	if strings.Contains(lower, "idx_one_pending_task") || strings.Contains(lower, "constraint") ||
		strings.Contains(lower, service.ErrDuplicatePendingTask.Error()) {
		t.Errorf("duplicate-coalesce response leaked raw sentinel/constraint detail: %q", body["error"])
	}
}

// TestWriteRerunIssueError_GenuineFailureKeepsRawMessage proves the
// classifier does not over-broaden: a real failure (not the typed duplicate
// sentinel) still surfaces as a 400 with its own message, so genuine errors
// stay visible instead of being folded into the coalesce path.
func TestWriteRerunIssueError_GenuineFailureKeepsRawMessage(t *testing.T) {
	w := httptest.NewRecorder()
	writeRerunIssueError(w, "issue-1", context.DeadlineExceeded)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, w.Body.String())
	}
	if body["error"] != context.DeadlineExceeded.Error() {
		t.Errorf("error = %q, want %q", body["error"], context.DeadlineExceeded.Error())
	}
}

// TestRerunIssue_DuplicatePendingTaskCoalesces is a real-contention probe of
// the full handler path (not just the classifier): under sustained
// concurrent writers hammering the same (issue, agent) pending slot, the
// zero-arg assignee-rerun call must end up either winning admission (202) or
// hitting the coalesce classification (409) — never any other status, and
// never a raw constraint leak on the 409 path. Contention timing means this
// run may or may not exhaust RerunIssue's reclaim budget, so it only asserts
// the invariant that holds on EITHER outcome; the deterministic classifier
// coverage above is what CI relies on for the 409 behavior itself.
func TestRerunIssue_DuplicatePendingTaskCoalesces(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "CHE-525 rerun contention runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "CHE-525 rerun contention agent")
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

	if w.Code != http.StatusAccepted && w.Code != http.StatusConflict {
		t.Fatalf("RerunIssue under slot contention: expected 202 or 409, got %d: %s", w.Code, w.Body.String())
	}
	if w.Code == http.StatusConflict {
		var body map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode conflict body: %v (body=%s)", err, w.Body.String())
		}
		if strings.Contains(strings.ToLower(body["error"]), "idx_one_pending_task") ||
			strings.Contains(strings.ToLower(body["error"]), "constraint") {
			t.Errorf("duplicate-coalesce response leaked raw constraint detail: %q", body["error"])
		}
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
