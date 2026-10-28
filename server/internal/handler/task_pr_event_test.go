package handler

import (
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

var prEventPRNumber atomic.Int32

// prEventFixture is a running issue task plus one accepted PR event on that issue.
type prEventFixture struct {
	supplementFixture
	eventID string
}

// newPREventFixture builds a running task that holds exactly the listed
// capabilities, then an accepted PR event on its issue.
func newPREventFixture(t *testing.T, capabilities ...string) prEventFixture {
	t.Helper()
	f := newSupplementFixture(t, "codex", "running", false)
	for _, capability := range capabilities {
		table := "task_supplement_capability"
		if capability == protocol.DaemonCapabilityTaskSupplementPREventV1 {
			table = "task_pr_event_capability"
		}
		dbfx.Exec(t, fmt.Sprintf(`INSERT INTO %s (task_id, workspace_id, issue_id, capability) VALUES ($1, $2, $3, $4)`, table),
			f.taskID, testWorkspaceID, f.issueID, capability)
		dbfx.Cleanup(t, fmt.Sprintf(`DELETE FROM %s WHERE task_id = $1`, table), f.taskID)
	}
	dbfx.Cleanup(t, `DELETE FROM task_pr_event_delivery WHERE task_id = $1`, f.taskID)

	number := 880000 + prEventPRNumber.Add(1)
	var prID string
	if err := testPool.QueryRow(t.Context(), `
		INSERT INTO github_pull_request (workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha)
		VALUES ($1, 1, 'multica-ai', 'multica', $2, 'SECRET PR TITLE', 'open', 'https://example.test/pr', now(), now(), 'new-head')
		RETURNING id
	`, testWorkspaceID, number).Scan(&prID); err != nil {
		t.Fatal(err)
	}
	dbfx.Cleanup(t, `DELETE FROM github_pull_request WHERE id = $1`, prID)
	dbfx.Cleanup(t, `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, prID)
	dbfx.Cleanup(t, `DELETE FROM issue_pr_event WHERE pull_request_id = $1`, prID)
	dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, f.issueID, prID)
	var eventID string
	if err := testPool.QueryRow(t.Context(), `
		INSERT INTO issue_pr_event (workspace_id, issue_id, pull_request_id, kind, head_sha, base_sha, observed_at, dedupe_key)
		VALUES ($1, $2, $3, 'head_changed', 'new-head', 'base-1', now(), 'head_changed|new-head')
		RETURNING id
	`, testWorkspaceID, f.issueID, prID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	return prEventFixture{supplementFixture: f, eventID: eventID}
}

func (f prEventFixture) enqueue(t *testing.T) error {
	t.Helper()
	_, err := testHandler.Queries.EnqueueTaskPREventDelivery(t.Context(), db.EnqueueTaskPREventDeliveryParams{
		TaskID: parseUUID(f.taskID), EventID: parseUUID(f.eventID),
	})
	return err
}

func (f prEventFixture) claim(t *testing.T) map[string]any {
	t.Helper()
	req := withURLParam(newOwnerDaemonRequest(http.MethodPost, "/claim", map[string]any{}), "taskId", f.taskID)
	var out map[string]any
	testutil.Call(t, testHandler.ClaimTaskPREvent, req).Want(http.StatusOK).JSON(&out)
	return out
}

func (f prEventFixture) ack(t *testing.T, delivered bool, want int) {
	t.Helper()
	req := withURLParams(newOwnerDaemonRequest(http.MethodPost, "/ack", map[string]any{"delivered": delivered}), "taskId", f.taskID, "eventId", f.eventID)
	testutil.Call(t, testHandler.AckTaskPREvent, req).Want(want)
}

func (f prEventFixture) eventStatus(t *testing.T) string {
	t.Helper()
	var status string
	if err := testPool.QueryRow(t.Context(), `SELECT status FROM issue_pr_event WHERE id = $1`, f.eventID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestTaskPREventNewCapabilityClaimHasNoMemberAuthor(t *testing.T) {
	f := newPREventFixture(t, protocol.DaemonCapabilityTaskSupplementPREventV1)
	if err := f.enqueue(t); err != nil {
		t.Fatal(err)
	}
	got := f.claim(t)
	if got["event_id"] != f.eventID || got["kind"] != "head_changed" || got["head_sha"] != "new-head" {
		t.Fatalf("claim = %v", got)
	}
	if _, ok := got["pr_number"]; !ok {
		t.Fatalf("claim lacks pr_number: %v", got)
	}
	for _, forbidden := range []string{"author_name", "content", "title", "body"} {
		if _, ok := got[forbidden]; ok {
			t.Fatalf("claim carries %q: %v", forbidden, got)
		}
	}
	if empty := f.claim(t); len(empty) != 0 {
		t.Fatalf("second claim = %v, want empty", empty)
	}
	f.ack(t, true, http.StatusOK)
	if s := f.eventStatus(t); s != "delivered" {
		t.Fatalf("event status = %q, want delivered", s)
	}
	f.ack(t, true, http.StatusOK) // replayed ack is idempotent
}

func TestTaskPREventOldHeadIgnoresSupersededObservation(t *testing.T) {
	f := newPREventFixture(t, protocol.DaemonCapabilityTaskSupplementPREventV1)
	var prID string
	if err := testPool.QueryRow(t.Context(), `SELECT pull_request_id FROM issue_pr_event WHERE id = $1`, f.eventID).Scan(&prID); err != nil {
		t.Fatal(err)
	}
	// A real earlier head, then a rejected stale webhook observed after it.
	for _, row := range []struct{ head, status, age string }{
		{"real-old-head", "delivered", "2 minutes"},
		{"stale-webhook-head", "superseded", "1 minute"},
	} {
		dbfx.Exec(t, `
			INSERT INTO issue_pr_event (workspace_id, issue_id, pull_request_id, kind, head_sha, observed_at, dedupe_key, status)
			VALUES ($1, $2, $3, 'head_changed', $4, now() - $5::interval, $6, $7)
		`, testWorkspaceID, f.issueID, prID, row.head, row.age, "head_changed|"+row.head, row.status)
	}
	if err := f.enqueue(t); err != nil {
		t.Fatal(err)
	}
	if got := f.claim(t)["old_head_sha"]; got != "real-old-head" {
		t.Fatalf("old_head_sha = %v, want real-old-head", got)
	}
}

func TestTaskPREventLegacyDaemonGetsNoMidRunDelivery(t *testing.T) {
	f := newPREventFixture(t, protocol.DaemonCapabilityTaskSupplementV1)
	if err := f.enqueue(t); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("enqueue on a task without the PR-event capability = %v, want no rows", err)
	}
	if got := f.claim(t); len(got) != 0 {
		t.Fatalf("claim = %v, want empty", got)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM task_pr_event_delivery WHERE task_id = $1`, f.taskID); n != 0 {
		t.Fatalf("delivery rows = %d, want 0", n)
	}
	if s := f.eventStatus(t); s != "accepted" {
		t.Fatalf("event status = %q, want accepted (left for the next-turn path)", s)
	}
}

func TestTaskPREventAndMemberCommentShareOneRun(t *testing.T) {
	f := newPREventFixture(t, protocol.DaemonCapabilityTaskSupplementV1, protocol.DaemonCapabilityTaskSupplementPREventV1)
	supplementRequest(t, f.supplementFixture, "0199a4e8-22ce-7b01-bba5-111111111111", "member guidance").Want(http.StatusCreated)
	if err := f.enqueue(t); err != nil {
		t.Fatal(err)
	}
	member := map[string]any{}
	req := withURLParam(newOwnerDaemonRequest(http.MethodPost, "/claim", map[string]any{}), "taskId", f.taskID)
	testutil.Call(t, testHandler.ClaimTaskSupplement, req).Want(http.StatusOK).JSON(&member)
	if member["content"] != "member guidance" || member["author_name"] == nil {
		t.Fatalf("member claim = %v", member)
	}
	if event := f.claim(t); event["event_id"] != f.eventID {
		t.Fatalf("PR event claim = %v", event)
	}
}

func TestTaskPREventStartLeavesSupplementCapabilityRowUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name         string
		offered      []string
		wantPREvent  bool
		wantSupplRow bool
	}{
		{"both offered", []string{protocol.DaemonCapabilityTaskSupplementV1, protocol.DaemonCapabilityTaskSupplementPREventV1}, true, true},
		{"legacy daemon", []string{protocol.DaemonCapabilityTaskSupplementV1}, false, true},
		{"pr event only", []string{protocol.DaemonCapabilityTaskSupplementPREventV1}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSupplementFixture(t, "codex", "dispatched", false)
			dbfx.Cleanup(t, `DELETE FROM task_pr_event_capability WHERE task_id = $1`, f.taskID)
			req := withURLParam(newRequest(http.MethodPost, "/start", map[string]any{"capabilities": tc.offered}), "taskId", f.taskID)
			var started AgentTaskResponse
			testutil.Call(t, testHandler.StartTask, req).Want(http.StatusOK).JSON(&started)
			if got := started.PREventCapability == protocol.DaemonCapabilityTaskSupplementPREventV1; got != tc.wantPREvent {
				t.Fatalf("pr_event_capability = %q, want present=%v", started.PREventCapability, tc.wantPREvent)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM task_pr_event_capability WHERE task_id = $1`, f.taskID); (n == 1) != tc.wantPREvent {
				t.Fatalf("pr event capability rows = %d, want present=%v", n, tc.wantPREvent)
			}
			var rows, supplement int
			if err := testPool.QueryRow(t.Context(), `
				SELECT count(*), count(*) FILTER (WHERE capability = 'task-supplement-v1')
				FROM task_supplement_capability WHERE task_id = $1`, f.taskID).Scan(&rows, &supplement); err != nil {
				t.Fatal(err)
			}
			wantRows := 0
			if tc.wantSupplRow {
				wantRows = 1
			}
			if rows != wantRows || supplement != wantRows {
				t.Fatalf("task_supplement_capability rows=%d supplement=%d, want %d of each", rows, supplement, wantRows)
			}
		})
	}
}

func TestTaskPREventTurnEndSettlesDeliveryToNextTurn(t *testing.T) {
	f := newPREventFixture(t, protocol.DaemonCapabilityTaskSupplementPREventV1)
	if err := f.enqueue(t); err != nil {
		t.Fatal(err)
	}
	if w := completeTaskViaHandler(t, f.taskID, "done"); w.Code != http.StatusOK {
		t.Fatalf("complete task: status %d: %s", w.Code, w.Body.String())
	}
	var status, reason string
	if err := testPool.QueryRow(t.Context(), `SELECT status, failure_reason FROM task_pr_event_delivery WHERE task_id = $1`, f.taskID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || reason != "turn_ended" {
		t.Fatalf("delivery = %s/%s, want failed/turn_ended", status, reason)
	}
	if s := f.eventStatus(t); s != "next_turn" {
		t.Fatalf("event status = %q, want next_turn", s)
	}
	if got := f.claim(t); len(got) != 0 {
		t.Fatalf("claim after turn end = %v, want empty", got)
	}
}

func TestTaskPREventFailedAckReturnsEventToNextTurn(t *testing.T) {
	f := newPREventFixture(t, protocol.DaemonCapabilityTaskSupplementPREventV1)
	if err := f.enqueue(t); err != nil {
		t.Fatal(err)
	}
	f.claim(t)
	f.ack(t, false, http.StatusOK)
	if s := f.eventStatus(t); s != "next_turn" {
		t.Fatalf("event status = %q, want next_turn", s)
	}
	f.ack(t, false, http.StatusConflict) // already failed: not deliverable again
}
