package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
)

func buildDequeuedPRWebhookBody(title string, prNumber int, installationID int64) map[string]any {
	body := buildMergedPRWebhookBody(title, prNumber, "acme", "widget", installationID)
	body["action"] = "dequeued"
	body["reason"] = "MERGE_CONFLICT"
	pr := body["pull_request"].(map[string]any)
	pr["state"], pr["merged"], pr["merged_at"], pr["closed_at"], pr["merge_commit_sha"] = "open", false, nil, nil, nil
	pr["head"].(map[string]any)["sha"] = "deq0123456789abcdef0123456789abcdef0123"
	pr["base"] = map[string]any{"ref": "main"}
	return body
}

type dequeuedWake struct {
	count               int
	pr                  float64
	headSHA, reason, ty string
}

func readDequeuedWake(t *testing.T, issueID string) dequeuedWake {
	t.Helper()
	var w dequeuedWake
	rows, err := testPool.Query(context.Background(), `SELECT r.event_type, r.payload FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1 AND w.system_rule='pr_dequeued'`, issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&w.ty, &raw); err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		w.count++
		w.pr, _ = p["pr_number"].(float64)
		w.headSHA, _ = p["head_sha"].(string)
		w.reason, _ = p["reason"].(string)
	}
	return w
}

func TestPRDequeuedWakesLinkedIssueWithFacts(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 423101
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-4231 dequeued", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)

	postSignedGitHubWebhook(t, fx.secret, buildDequeuedPRWebhookBody(fx.child.Identifier+": queued", 423111, installationID), "guid-4231-a")

	got := readDequeuedWake(t, fx.child.ID)
	if got.count != 1 || got.ty != "pr.dequeued" || got.pr != 423111 || got.reason != "MERGE_CONFLICT" || got.headSHA != "deq0123456789abcdef0123456789abcdef0123" {
		t.Fatalf("dequeued wake = %+v, want one pr.dequeued receipt with PR number, head SHA and reason", got)
	}
	var runs int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'='pr_dequeued'`, fx.child.ID).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("dequeued runs = %d (err %v), want 1", runs, err)
	}
}

func TestPRDequeuedDuplicateDeliveryWakesOnce(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 423102
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-4231 dequeued dup", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	body := buildDequeuedPRWebhookBody(fx.child.Identifier+": queued", 423112, installationID)

	postSignedGitHubWebhook(t, fx.secret, body, "guid-4231-dup")
	postSignedGitHubWebhook(t, fx.secret, body, "guid-4231-dup")
	if got := readDequeuedWake(t, fx.child.ID); got.count != 1 {
		t.Fatalf("same delivery GUID twice: %d receipts, want 1", got.count)
	}
	// The delivery header is unsigned. The same signed body resent under a new
	// delivery ID is a replay, not a new dequeue (CHE-4231 security finding).
	postSignedGitHubWebhook(t, fx.secret, body, "guid-4231-other")
	if got := readDequeuedWake(t, fx.child.ID); got.count != 1 {
		t.Fatalf("same signed body under a new delivery ID: %d receipts, want 1", got.count)
	}
	// A later dequeue is a different signed body (new updated_at), so it wakes again.
	body["pull_request"].(map[string]any)["updated_at"] = "2026-11-06T05:00:00Z"
	postSignedGitHubWebhook(t, fx.secret, body, "guid-4231-later")
	if got := readDequeuedWake(t, fx.child.ID); got.count != 2 {
		t.Fatalf("a later dequeue: %d receipts, want 2", got.count)
	}
}

func TestPRDequeuedUnlinkedOrUnsignedWakesNobody(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 423103
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-4231 dequeued none", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)

	// Unlinked: the PR names no issue.
	postSignedGitHubWebhook(t, fx.secret, buildDequeuedPRWebhookBody("no issue here", 423113, installationID), "guid-4231-unlinked")
	if got := readDequeuedWake(t, fx.child.ID); got.count != 0 {
		t.Fatalf("unlinked payload woke the issue: %+v", got)
	}

	// Unsigned: a linked payload with a bad signature is refused.
	raw, _ := json.Marshal(buildDequeuedPRWebhookBody(fx.child.Identifier+": queued", 423114, installationID))
	req := httptest.NewRequest("POST", "/api/webhooks/github", bytes.NewReader(raw))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", "sha256=00")
	req.Header.Set("X-GitHub-Delivery", "guid-4231-unsigned")
	rec := httptest.NewRecorder()
	testHandler.HandleGitHubWebhook(rec, req)
	if rec.Code != 401 {
		t.Fatalf("unsigned status = %d, want 401", rec.Code)
	}
	if got := readDequeuedWake(t, fx.child.ID); got.count != 0 {
		t.Fatalf("unsigned payload woke the issue: %+v", got)
	}
}

func TestPRDequeuedConditionFiresOncePerDequeue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 423104
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-4231 dequeued condition", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	ruleID := createRule(t, fx.child.ID, agentID, pgtype.UUID{}, service.WakeupInput{
		Instruction: "Handle dequeue", Kind: "event",
		Condition: json.RawMessage(`{"type":"pull_request","event":"dequeued"}`),
	})
	// A met condition records one receipt; the run itself may join the system
	// wake's pending run for the same handler, so the receipt is the signal.
	met := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND event_type='condition.met'`, ruleID)
	}
	due := func() {
		dbfx.Exec(t, `UPDATE issue_wakeup SET next_fire_at=now()-interval '1 minute' WHERE id=$1`, ruleID)
		runWakeupTick(t)
	}

	due()
	if got := met(); got != 0 {
		t.Fatalf("before any dequeue: %d, want 0", got)
	}
	body := buildDequeuedPRWebhookBody(fx.child.Identifier+": queued", 423115, installationID)
	postSignedGitHubWebhook(t, fx.secret, body, "guid-4231-cond-1")
	due()
	due()
	if got := met(); got != 1 {
		t.Fatalf("after one dequeue: %d, want 1", got)
	}
	body["pull_request"].(map[string]any)["updated_at"] = "2026-11-06T05:00:00Z"
	postSignedGitHubWebhook(t, fx.secret, body, "guid-4231-cond-2")
	due()
	if got := met(); got != 2 {
		t.Fatalf("after a second dequeue: %d, want 2", got)
	}
}

// A dequeue saved before an operator halts dispatch must not start a run while
// the repository is halted, nor replay after the halt lifts (CHE-4231 security).
func TestPRDequeuedConditionHonorsDispatchHalt(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 423105
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-4231 dequeued halt", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	ruleID := createRule(t, fx.child.ID, agentID, pgtype.UUID{}, service.WakeupInput{
		Instruction: "Handle dequeue", Kind: "event",
		Condition: json.RawMessage(`{"type":"pull_request","event":"dequeued"}`),
	})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_dispatch_halt WHERE workspace_id = $1`, testWorkspaceID)
	})
	met := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND event_type='condition.met'`, ruleID)
	}
	due := func() {
		dbfx.Exec(t, `UPDATE issue_wakeup SET next_fire_at=now()-interval '1 minute' WHERE id=$1`, ruleID)
		runWakeupTick(t)
	}

	body := buildDequeuedPRWebhookBody(fx.child.Identifier+": queued", 423116, installationID)
	postSignedGitHubWebhook(t, fx.secret, body, "guid-4231-halt-1")
	setHalt(t, "acme/widget", true, false)
	due()
	if got := met(); got != 0 {
		t.Fatalf("halted repo: dequeue condition fired %d times, want 0", got)
	}
	setHalt(t, "acme/widget", false, false)
	due()
	if got := met(); got != 0 {
		t.Fatalf("resume replayed the dropped dequeue: %d, want 0", got)
	}
}
