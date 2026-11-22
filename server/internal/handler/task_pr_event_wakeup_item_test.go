package handler

import (
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-6988 (plan CHE-6971, SA-1): wakeup items on the typed PR-event channel.
// Enqueue, claim, ack and fallback admission. PR items must behave as before.

type wakeupItemFixture struct {
	supplementFixture
	wakeupID   string
	receiptIDs []string
	eventID    string // firing id of a seeded item
}

// newWakeupItemFixture builds a running task holding the given capability, a
// condition wakeup on its issue and two pending receipts. No item exists yet.
func newWakeupItemFixture(t *testing.T, capability string) wakeupItemFixture {
	t.Helper()
	f := newSupplementFixture(t, "codex", "running", false)
	table := "task_pr_event_capability"
	dbfx.Exec(t, `INSERT INTO `+table+` (task_id, workspace_id, issue_id, capability) VALUES ($1, $2, $3, $4)`,
		f.taskID, testWorkspaceID, f.issueID, capability)
	dbfx.Cleanup(t, `DELETE FROM `+table+` WHERE task_id = $1`, f.taskID)
	dbfx.Cleanup(t, `DELETE FROM task_pr_event_delivery WHERE task_id = $1`, f.taskID)

	w := wakeupItemFixture{supplementFixture: f}
	dbfx.QueryRow(t, `INSERT INTO issue_wakeup (id, workspace_id, issue_id, agent_id, instruction, kind, mode, event_types, timezone)
		VALUES (gen_random_uuid(), $1, $2, $3, 'check the PR', 'event', 'once', '{}', 'UTC') RETURNING id`,
		testWorkspaceID, f.issueID, f.agentID).Scan(&w.wakeupID)
	dbfx.Cleanup(t, `DELETE FROM issue_wakeup_receipt WHERE wakeup_id = $1`, w.wakeupID)
	dbfx.Cleanup(t, `DELETE FROM issue_wakeup WHERE id = $1`, w.wakeupID)
	for _, key := range []string{"a", "b"} {
		var id string
		dbfx.QueryRow(t, `INSERT INTO issue_wakeup_receipt (id, wakeup_id, revision, event_key, event_type, payload)
			VALUES (gen_random_uuid(), $1, 1, $2, 'condition.met', '{}'::jsonb) RETURNING id`, w.wakeupID, key).Scan(&id)
		w.receiptIDs = append(w.receiptIDs, id)
	}
	return w
}

// seed inserts a wakeup item directly, bypassing EnqueueTaskWakeupDelivery.
func (w wakeupItemFixture) seed(t *testing.T, status, reason string, attempts int, age string) wakeupItemFixture {
	t.Helper()
	dbfx.QueryRow(t, `INSERT INTO task_pr_event_delivery
		(task_id, event_id, workspace_id, issue_id, status, failure_reason, attempt_count, created_at, updated_at,
		 item_kind, wakeup_id, wakeup_revision, receipt_ids, note)
		VALUES ($1, gen_random_uuid(), $2, $3, $4, NULLIF($5, ''), $6, now() - $7::interval, now() - $7::interval,
		 'wakeup', $8, 1, $9::uuid[], 'wakeup note')
		RETURNING event_id`,
		w.taskID, testWorkspaceID, w.issueID, status, reason, attempts, age, w.wakeupID, w.receiptIDs).Scan(&w.eventID)
	return w
}

func (w wakeupItemFixture) claim(t *testing.T) map[string]any {
	t.Helper()
	req := withURLParam(newOwnerDaemonRequest(http.MethodPost, "/claim", map[string]any{}), "taskId", w.taskID)
	var out map[string]any
	testutil.Call(t, testHandler.ClaimTaskPREvent, req).Want(http.StatusOK).JSON(&out)
	return out
}

func (w wakeupItemFixture) ack(t *testing.T, delivered bool, want int) {
	t.Helper()
	req := withURLParams(newOwnerDaemonRequest(http.MethodPost, "/ack", map[string]any{"delivered": delivered}), "taskId", w.taskID, "eventId", w.eventID)
	testutil.Call(t, testHandler.AckTaskPREvent, req).Want(want)
}

func (w wakeupItemFixture) status(t *testing.T) string {
	t.Helper()
	var s string
	dbfx.QueryRow(t, `SELECT status FROM task_pr_event_delivery WHERE task_id = $1 AND event_id = $2`, w.taskID, w.eventID).Scan(&s)
	return s
}

func (w wakeupItemFixture) enqueue(t *testing.T, receipts ...string) (db.TaskPrEventDelivery, error) {
	t.Helper()
	ids := make([]pgtype.UUID, len(receipts))
	for i, r := range receipts {
		ids[i] = parseUUID(r)
	}
	return testHandler.Queries.EnqueueTaskWakeupDelivery(t.Context(), db.EnqueueTaskWakeupDeliveryParams{
		TaskID: parseUUID(w.taskID), WakeupID: parseUUID(w.wakeupID), WakeupRevision: 1,
		ReceiptIds: ids, Note: "wakeup note",
	})
}

func (w wakeupItemFixture) admit(t *testing.T, seen, reason string) error {
	t.Helper()
	_, err := testHandler.Queries.AdmitWakeupDeliveryFallback(t.Context(), db.AdmitWakeupDeliveryFallbackParams{
		WakeupID: parseUUID(w.wakeupID), EventID: parseUUID(w.eventID), SeenStatus: seen, Reason: reason,
	})
	return err
}

// Red test: a seeded pending wakeup item on a v2 task is claimed with the wakeup body.
func TestWakeupItemClaimedByV2Task(t *testing.T) {
	w := newWakeupItemFixture(t, protocol.DaemonCapabilityTaskSupplementPREventV2).seed(t, "pending", "", 0, "0 seconds")
	got := w.claim(t)
	want := map[string]any{"event_id": w.eventID, "kind": "wakeup", "wakeup_id": w.wakeupID, "text": "wakeup note"}
	if len(got) != len(want) {
		t.Fatalf("claim = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("claim[%q] = %v, want %v (claim %v)", k, got[k], v, got)
		}
	}
	if s := w.status(t); s != "delivering" {
		t.Fatalf("item status after claim = %q, want delivering", s)
	}
	if again := w.claim(t); len(again) != 0 {
		t.Fatalf("second claim = %v, want empty", again)
	}
}

func TestWakeupItemNeverClaimedByV1Task(t *testing.T) {
	w := newWakeupItemFixture(t, protocol.DaemonCapabilityTaskSupplementPREventV1).seed(t, "pending", "", 0, "0 seconds")
	if got := w.claim(t); len(got) != 0 {
		t.Fatalf("v1 task claimed a wakeup item: %v", got)
	}
	if s := w.status(t); s != "pending" {
		t.Fatalf("item status = %q, want pending", s)
	}
}

// The claim refuses an edited, paused or workspace-paused wakeup and an item
// whose receipt was discarded or reserved. Each case restores the condition and
// then claims, so the refusal is the only thing that changed.
func TestWakeupItemClaimRefusals(t *testing.T) {
	for _, tc := range []struct{ name, break_, restore string }{
		{"edited wakeup", `UPDATE issue_wakeup SET revision = 2 WHERE id = $1`, `UPDATE issue_wakeup SET revision = 1 WHERE id = $1`},
		{"paused wakeup", `UPDATE issue_wakeup SET paused_reason = 'loop' WHERE id = $1`, `UPDATE issue_wakeup SET paused_reason = NULL WHERE id = $1`},
		{"discarded receipt", `DELETE FROM issue_wakeup_receipt WHERE wakeup_id = $1 AND event_key = 'a'`, ``},
		{"processed receipt", `UPDATE issue_wakeup_receipt SET processed_at = now() WHERE wakeup_id = $1 AND event_key = 'a'`, `UPDATE issue_wakeup_receipt SET processed_at = NULL WHERE wakeup_id = $1`},
		{"reserved receipt", `UPDATE issue_wakeup_receipt SET task_id = gen_random_uuid() WHERE wakeup_id = $1 AND event_key = 'b'`, `UPDATE issue_wakeup_receipt SET task_id = NULL WHERE wakeup_id = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWakeupItemFixture(t, protocol.DaemonCapabilityTaskSupplementPREventV2).seed(t, "pending", "", 0, "0 seconds")
			dbfx.Exec(t, tc.break_, w.wakeupID)
			if got := w.claim(t); len(got) != 0 {
				t.Fatalf("claimed despite %s: %v", tc.name, got)
			}
			if s := w.status(t); s != "pending" {
				t.Fatalf("item status = %q, want pending", s)
			}
			if tc.restore == "" {
				return // a discarded receipt does not come back
			}
			dbfx.Exec(t, tc.restore, w.wakeupID)
			if got := w.claim(t); got["kind"] != "wakeup" {
				t.Fatalf("not claimed after the condition was restored: %v", got)
			}
		})
	}
	t.Run("workspace paused", func(t *testing.T) {
		w := newWakeupItemFixture(t, protocol.DaemonCapabilityTaskSupplementPREventV2).seed(t, "pending", "", 0, "0 seconds")
		var original string
		dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&original)
		dbfx.Cleanup(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, original)
		dbfx.Exec(t, `UPDATE workspace SET settings = settings || '{"wakeups_paused": true}'::jsonb WHERE id = $1`, testWorkspaceID)
		if got := w.claim(t); len(got) != 0 {
			t.Fatalf("claimed while the workspace is paused: %v", got)
		}
		dbfx.Exec(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, original)
		if got := w.claim(t); got["kind"] != "wakeup" {
			t.Fatalf("not claimed after the workspace resumed: %v", got)
		}
	})
}

func TestWakeupItemClaimOrdersBothKindsByCreatedAt(t *testing.T) {
	f := newPREventFixture(t)
	dbfx.Exec(t, `INSERT INTO task_pr_event_capability (task_id, workspace_id, issue_id, capability) VALUES ($1, $2, $3, $4)`,
		f.taskID, testWorkspaceID, f.issueID, protocol.DaemonCapabilityTaskSupplementPREventV2)
	dbfx.Cleanup(t, `DELETE FROM task_pr_event_capability WHERE task_id = $1`, f.taskID)
	w := newWakeupItemFixtureOn(t, f.supplementFixture).seed(t, "pending", "", 0, "1 hour") // older than the PR item
	if err := f.enqueue(t); err != nil {
		t.Fatal(err)
	}
	if got := w.claim(t); got["kind"] != "wakeup" {
		t.Fatalf("first claim = %v, want the older wakeup item", got)
	}
	if got := w.claim(t); got["kind"] != "head_changed" || got["head_sha"] != "new-head" {
		t.Fatalf("second claim = %v, want the PR item", got)
	}
}

// newWakeupItemFixtureOn adds the wakeup and receipts to an existing task.
func newWakeupItemFixtureOn(t *testing.T, f supplementFixture) wakeupItemFixture {
	t.Helper()
	w := wakeupItemFixture{supplementFixture: f}
	dbfx.QueryRow(t, `INSERT INTO issue_wakeup (id, workspace_id, issue_id, agent_id, instruction, kind, mode, event_types, timezone)
		VALUES (gen_random_uuid(), $1, $2, $3, 'check the PR', 'event', 'once', '{}', 'UTC') RETURNING id`,
		testWorkspaceID, f.issueID, f.agentID).Scan(&w.wakeupID)
	dbfx.Cleanup(t, `DELETE FROM issue_wakeup_receipt WHERE wakeup_id = $1`, w.wakeupID)
	dbfx.Cleanup(t, `DELETE FROM issue_wakeup WHERE id = $1`, w.wakeupID)
	var id string
	dbfx.QueryRow(t, `INSERT INTO issue_wakeup_receipt (id, wakeup_id, revision, event_key, event_type, payload)
		VALUES (gen_random_uuid(), $1, 1, 'a', 'condition.met', '{}'::jsonb) RETURNING id`, w.wakeupID).Scan(&id)
	w.receiptIDs = []string{id}
	return w
}

func TestEnqueueTaskWakeupDelivery(t *testing.T) {
	v2 := protocol.DaemonCapabilityTaskSupplementPREventV2
	t.Run("inserts a pending item with a fresh firing id and no PR event", func(t *testing.T) {
		w := newWakeupItemFixture(t, v2)
		row, err := w.enqueue(t, w.receiptIDs...)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if row.Status != "pending" || row.ItemKind != "wakeup" || row.Note.String != "wakeup note" ||
			uuidToString(row.WakeupID) != w.wakeupID || len(row.ReceiptIds) != 2 || row.WakeupRevision.Int32 != 1 {
			t.Fatalf("item = %+v", row)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM issue_pr_event WHERE id = $1`, uuidToString(row.EventID)); n != 0 {
			t.Fatalf("a wakeup item created %d issue_pr_event rows", n)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id = $1 AND (task_id IS NOT NULL OR processed_at IS NOT NULL)`, w.wakeupID); n != 0 {
			t.Fatalf("enqueue reserved or processed %d receipts", n)
		}
	})
	t.Run("refuses a reused receipt id", func(t *testing.T) {
		w := newWakeupItemFixture(t, v2)
		if _, err := w.enqueue(t, w.receiptIDs[0]); err != nil {
			t.Fatalf("first enqueue: %v", err)
		}
		if _, err := w.enqueue(t, w.receiptIDs[0], w.receiptIDs[1]); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("overlapping enqueue err = %v, want no rows", err)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM task_pr_event_delivery WHERE task_id = $1`, w.taskID); n != 1 {
			t.Fatalf("items = %d, want 1", n)
		}
		if _, err := w.enqueue(t, w.receiptIDs[1]); err != nil {
			t.Fatalf("disjoint enqueue: %v", err)
		}
	})
	t.Run("refuses a task that is not running", func(t *testing.T) {
		w := newWakeupItemFixture(t, v2)
		dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, w.taskID)
		if _, err := w.enqueue(t, w.receiptIDs...); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("err = %v, want no rows", err)
		}
	})
	t.Run("refuses a v1 task", func(t *testing.T) {
		w := newWakeupItemFixture(t, protocol.DaemonCapabilityTaskSupplementPREventV1)
		if _, err := w.enqueue(t, w.receiptIDs...); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("err = %v, want no rows", err)
		}
	})
}

func TestWakeupItemAckDeliveredWritesNoPREventOrReceipt(t *testing.T) {
	w := newWakeupItemFixture(t, protocol.DaemonCapabilityTaskSupplementPREventV2).seed(t, "pending", "", 0, "0 seconds")
	if got := w.claim(t); got["kind"] != "wakeup" {
		t.Fatalf("claim = %v", got)
	}
	w.ack(t, true, http.StatusOK)
	if s := w.status(t); s != "delivered" {
		t.Fatalf("status = %q, want delivered", s)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_pr_event WHERE issue_id = $1`, w.issueID); n != 0 {
		t.Fatalf("ack wrote %d issue_pr_event rows", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id = $1 AND (task_id IS NOT NULL OR processed_at IS NOT NULL)`, w.wakeupID); n != 0 {
		t.Fatalf("ack changed %d receipts", n)
	}
	w.ack(t, true, http.StatusOK) // replay stays accepted
}

func TestWakeupItemLateAckAfterTurnEndedBecomesDelivered(t *testing.T) {
	w := newWakeupItemFixture(t, protocol.DaemonCapabilityTaskSupplementPREventV2).seed(t, "failed", "turn_ended", 1, "5 seconds")
	w.ack(t, true, http.StatusOK)
	if s := w.status(t); s != "delivered" {
		t.Fatalf("status = %q, want delivered", s)
	}
}

// Never claimed: no ack can win.
func TestWakeupItemAckAfterTurnEndedWithoutClaimReturnsNoRow(t *testing.T) {
	w := newWakeupItemFixture(t, protocol.DaemonCapabilityTaskSupplementPREventV2).seed(t, "failed", "turn_ended", 0, "5 seconds")
	w.ack(t, true, http.StatusConflict)
}

func TestWakeupItemAckAfterFallbackReturnsNoRow(t *testing.T) {
	w := newWakeupItemFixture(t, protocol.DaemonCapabilityTaskSupplementPREventV2).seed(t, "fallback", "turn_ended", 1, "5 seconds")
	w.ack(t, true, http.StatusConflict)
	if s := w.status(t); s != "fallback" {
		t.Fatalf("status = %q, want fallback", s)
	}
}

func TestSettleTerminalFailsWakeupItems(t *testing.T) {
	w := newWakeupItemFixture(t, protocol.DaemonCapabilityTaskSupplementPREventV2)
	pending := w.seed(t, "pending", "", 0, "0 seconds")
	delivering := w.seed(t, "delivering", "", 1, "0 seconds")
	delivered := w.seed(t, "delivered", "", 1, "0 seconds")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, w.taskID)
	if _, err := testHandler.Queries.SettleTerminalTaskPREventDeliveries(t.Context(), []pgtype.UUID{parseUUID(w.taskID)}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		f    wakeupItemFixture
		want string
	}{{pending, "failed"}, {delivering, "failed"}, {delivered, "delivered"}} {
		if s := c.f.status(t); s != c.want {
			t.Fatalf("item %s status = %q, want %q", c.f.eventID, s, c.want)
		}
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM task_pr_event_delivery WHERE task_id = $1 AND status = 'failed' AND failure_reason = 'turn_ended'`, w.taskID); n != 2 {
		t.Fatalf("turn_ended items = %d, want 2", n)
	}
}

func TestAdmitWakeupDeliveryFallback(t *testing.T) {
	v2 := protocol.DaemonCapabilityTaskSupplementPREventV2
	type tc struct {
		name, status, failure string
		attempts              int
		age, taskStatus       string
		seen, reason          string
		want                  bool
	}
	for _, c := range []tc{
		{"unclaimed 120s", "pending", "", 0, "121 seconds", "running", "pending", "unclaimed", true},
		{"unclaimed under 120s", "pending", "", 0, "119 seconds", "running", "pending", "unclaimed", false},
		{"turn_ended never claimed", "failed", "turn_ended", 0, "1 second", "completed", "failed", "turn_ended", true},
		{"turn_ended claimed holds for the late ack", "failed", "turn_ended", 1, "1 second", "completed", "failed", "turn_ended", false},
		{"late ack cutoff", "failed", "turn_ended", 1, "61 seconds", "completed", "failed", "late_ack_cutoff", true},
		{"inside the late ack grace", "failed", "turn_ended", 1, "59 seconds", "completed", "failed", "late_ack_cutoff", false},
		{"provider rejected", "failed", "provider_rejected", 1, "1 second", "running", "failed", "rejected", true},
		{"turn_ended is not a rejection", "failed", "turn_ended", 1, "1 second", "running", "failed", "rejected", false},
		{"delivered, host cancelled", "delivered", "", 1, "1 second", "cancelled", "delivered", "host_cancelled", true},
		{"delivered, host failed", "delivered", "", 1, "1 second", "failed", "delivered", "host_failed", true},
		{"delivered, host running", "delivered", "", 1, "1 hour", "running", "delivered", "host_cancelled", false},
		{"delivered, host running (failed reason)", "delivered", "", 1, "1 hour", "running", "delivered", "host_failed", false},
		{"delivered, host completed", "delivered", "", 1, "1 hour", "completed", "delivered", "host_cancelled", false},
		{"delivered, host completed (failed reason)", "delivered", "", 1, "1 hour", "completed", "delivered", "host_failed", false},
		{"cancelled host does not match host_failed", "delivered", "", 1, "1 second", "cancelled", "delivered", "host_failed", false},
		{"delivered item is not admitted from pending", "delivered", "", 1, "1 hour", "cancelled", "pending", "unclaimed", false},
		{"delivered item is not admitted from failed", "delivered", "", 1, "1 hour", "cancelled", "failed", "turn_ended", false},
		{"seen status must match", "pending", "", 0, "300 seconds", "running", "failed", "unclaimed", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWakeupItemFixture(t, v2).seed(t, c.status, c.failure, c.attempts, c.age)
			switch c.taskStatus {
			case "completed", "cancelled", "failed":
				dbfx.Exec(t, `UPDATE agent_task_queue SET status = $2, completed_at = now() WHERE id = $1`, w.taskID, c.taskStatus)
			}
			err := w.admit(t, c.seen, c.reason)
			if c.want {
				if err != nil {
					t.Fatalf("admit: %v", err)
				}
				if s := w.status(t); s != "fallback" {
					t.Fatalf("status = %q, want fallback", s)
				}
				if again := w.admit(t, c.seen, c.reason); !errors.Is(again, pgx.ErrNoRows) {
					t.Fatalf("second admit err = %v, want no rows", again)
				}
				return
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("admit err = %v, want no rows", err)
			}
			if s := w.status(t); s != c.status {
				t.Fatalf("status = %q, want unchanged %q", s, c.status)
			}
		})
	}
	t.Run("never admits a PR item", func(t *testing.T) {
		f := newPREventFixture(t)
		dbfx.Exec(t, `INSERT INTO task_pr_event_capability (task_id, workspace_id, issue_id, capability) VALUES ($1, $2, $3, $4)`,
			f.taskID, testWorkspaceID, f.issueID, v2)
		dbfx.Cleanup(t, `DELETE FROM task_pr_event_capability WHERE task_id = $1`, f.taskID)
		if err := f.enqueue(t); err != nil {
			t.Fatal(err)
		}
		_, err := testHandler.Queries.AdmitWakeupDeliveryFallback(t.Context(), db.AdmitWakeupDeliveryFallbackParams{
			WakeupID: pgtype.UUID{}, EventID: parseUUID(f.eventID), SeenStatus: "pending", Reason: "unclaimed",
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("err = %v, want no rows", err)
		}
	})
}

func TestListWakeupDeliveryItemsCarriesHostRunStatus(t *testing.T) {
	w := newWakeupItemFixture(t, protocol.DaemonCapabilityTaskSupplementPREventV2).seed(t, "delivered", "", 1, "1 second")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'failed', failure_reason = 'agent_error', completed_at = now() WHERE id = $1`, w.taskID)
	rows, err := testHandler.Queries.ListWakeupDeliveryItems(t.Context(), parseUUID(w.wakeupID))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || uuidToString(rows[0].EventID) != w.eventID || rows[0].Status != "delivered" ||
		rows[0].TaskStatus != "failed" || rows[0].TaskFailureReason.String != "agent_error" {
		t.Fatalf("rows = %+v", rows)
	}
}
