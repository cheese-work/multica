package handler

import (
	"context"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-2298 d3: the a3 fallback and the c1 stop selector agree on pr.event
// receipts; draft and ready never start a run.

// capture stores one PR event through the production capture path.
func (w c1World) capture(t *testing.T, kind ghsnapshot.PREventKind) {
	t.Helper()
	if _, err := ghsnapshot.CapturePREvent(t.Context(), testHandler.Queries, ghsnapshot.PREventInput{
		PRID: parseUUID(w.prID), Kind: kind, HeadSHA: "c1head", BaseSHA: "base-1", ObservedAt: time.Now(),
	}); err != nil {
		t.Fatalf("capture %s: %v", kind, err)
	}
}

// tick is the scheduler pass that bridges stored PR events into wake receipts.
func tick(t *testing.T) {
	t.Helper()
	if err := (&service.IssueWakeupService{Tasks: testHandler.TaskService}).TickWorkspaces(context.Background(), parseUUID(testWorkspaceID)); err != nil {
		t.Fatalf("tick wakeup scheduler: %v", err)
	}
}

// bridgedRuns lists the issue's runs started by a pr.event receipt.
func (w c1World) bridgedRuns(t *testing.T) []string {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT DISTINCT r.task_id::text FROM issue_wakeup_receipt r JOIN issue_wakeup k ON k.id = r.wakeup_id
		WHERE k.issue_id = $1 AND r.event_type = 'pr.event' AND r.task_id IS NOT NULL`, w.issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func (w c1World) assignAgent(t *testing.T) {
	t.Helper()
	setIssueAssigneeDirect(t, w.issueID, "agent", w.agentID)
}

func TestPREventReceiptRunStopsWhenPRMergesOrCloses(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		merged       bool
	}{{"merged", "pr_merged", true}, {"closed", "pr_closed", false}} {
		t.Run(tc.name, func(t *testing.T) {
			w := newC1World(t)
			w.assignAgent(t)
			w.capture(t, ghsnapshot.PREventConflict)
			tick(t)
			runs := w.bridgedRuns(t)
			if len(runs) != 1 {
				t.Fatalf("a3 bridge started %d runs, want 1", len(runs))
			}
			dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', dispatched_at = now(), started_at = now() WHERE id = $1`, runs[0])
			// No assignee: the merge wake would otherwise wait on the stop gate this test inspects.
			dbfx.Exec(t, `UPDATE issue SET assignee_type = NULL, assignee_id = NULL WHERE id = $1`, w.issueID)

			w.fire(t, "closed", "closed", tc.merged, false)

			if s, r := taskState(t, runs[0]); s != "cancelled" || r != tc.reason {
				t.Fatalf("pr.event run = %s/%q, want cancelled/%q", s, r, tc.reason)
			}
			stopGateClosedFor(t, w.issueID, runs[0])
			var runtimeID string
			dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent_task_queue WHERE id = $1`, runs[0]).Scan(&runtimeID)
			ackCancelled(t, runs[0], map[string]any{
				"cancel_ack_proof": auth.CancelAckProof(runs[0], runtimeID),
				"durable_work_dir": "/tmp/d3-preserved",
			})
			var dir *string
			dbfx.QueryRow(t, `SELECT durable_work_dir FROM agent_task_queue WHERE id = $1`, runs[0]).Scan(&dir)
			if dir == nil || *dir != "/tmp/d3-preserved" {
				t.Fatalf("durable_work_dir after ack = %v, want /tmp/d3-preserved", dir)
			}
			stopGateOpen(t, w.issueID)
		})
	}
}

// A legacy PR receipt run is still stopped; a run that also serves another
// purpose, another PR, or the merge itself is not.
func TestPRStopSelectsOnlyPROnlyReceipts(t *testing.T) {
	w := newC1World(t)
	legacy := w.prWakeRun(t, "running", "pr.checks_failed", ":c1head")

	mixed := w.prWakeRun(t, "running", "pr.event", "")
	dbfx.Exec(t, `UPDATE issue_wakeup_receipt SET event_key = 'pr_event:e1:a1', payload = jsonb_build_object('kind', 'conflict', 'repo_owner', 'acme', 'repo_name', $2::text, 'pr_number', $3::int) WHERE task_id = $1`, mixed, w.repo, w.number)
	dbfx.Exec(t, `INSERT INTO issue_wakeup_receipt (id, wakeup_id, revision, event_key, event_type, payload, task_id)
		SELECT gen_random_uuid(), wakeup_id, 1, 'other:1', 'comment.created', '{}'::jsonb, task_id FROM issue_wakeup_receipt WHERE task_id = $1 LIMIT 1`, mixed)

	otherPR := w.prWakeRun(t, "running", "pr.event", "")
	dbfx.Exec(t, `UPDATE issue_wakeup_receipt SET event_key = 'pr_event:e2:a1', payload = jsonb_build_object('kind', 'conflict', 'repo_owner', 'acme', 'repo_name', $2::text, 'pr_number', $3::int) WHERE task_id = $1`, otherPR, w.repo, w.number+1)

	postMerge := w.prWakeRun(t, "running", "pr.event", "")
	dbfx.Exec(t, `UPDATE issue_wakeup_receipt SET event_key = 'pr_event:e3:a1', payload = jsonb_build_object('kind', 'merged', 'repo_owner', 'acme', 'repo_name', $2::text, 'pr_number', $3::int) WHERE task_id = $1`, postMerge, w.repo, w.number)

	w.fire(t, "closed", "closed", true, false)

	if s, _ := taskState(t, legacy); s != "cancelled" {
		t.Fatalf("legacy PR receipt run = %s, want cancelled", s)
	}
	for name, id := range map[string]string{"mixed-purpose": mixed, "other-PR": otherPR, "post-merge": postMerge} {
		if s, _ := taskState(t, id); s != "running" {
			t.Fatalf("%s run = %s, want running", name, s)
		}
	}
}

// Draft and ready only notify a live run: with no run, or a run that cannot take
// the notice, they stay accepted and no run starts.
func TestPRDraftReadyNeverStartRunsThroughTheFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		live bool
	}{{"no live run", false}, {"legacy live run", true}} {
		t.Run(tc.name, func(t *testing.T) {
			w := newC1World(t)
			w.assignAgent(t)
			if tc.live {
				dbfx.Task(t, w.agentID, testutil.Cols{
					"issue_id": w.issueID, "runtime_id": w.runtimeID, "status": "running",
					"dispatched_at": testutil.Raw("now()"), "started_at": testutil.Raw("now()"),
				})
			}
			before := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, w.issueID)

			w.fire(t, "converted_to_draft", "open", false, true)
			w.fire(t, "ready_for_review", "open", false, false)
			tick(t)
			tick(t)

			if after := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, w.issueID); after != before {
				t.Fatalf("tasks on the issue = %d, want %d: draft/ready must not start a run", after, before)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup k ON k.id = r.wakeup_id WHERE k.issue_id = $1 AND r.event_type = 'pr.event'`, w.issueID); n != 0 {
				t.Fatalf("pr.event receipts = %d, want 0", n)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM issue_pr_event WHERE pull_request_id = $1 AND kind IN ('draft','ready') AND status = 'accepted'`, w.prID); n != 2 {
				t.Fatalf("accepted draft/ready events = %d, want 2: they stay visible and undelivered", n)
			}
		})
	}
}
