package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// supersededRun plants a run that was cancelled as superseded at the given
// step. Overrides tweak the row (acked, never dispatched, plain cancel).
func supersededRun(t *testing.T, agentID, runtimeID, issueID, step string, over ...testutil.Cols) string {
	t.Helper()
	cols := testutil.Cols{
		"runtime_id":     runtimeID,
		"issue_id":       issueID,
		"status":         "cancelled",
		"failure_reason": service.FailureReasonSupersededHead,
		"workflow_step":  step,
		"dispatched_at":  testutil.Raw("now() - interval '10 minutes'"),
		"started_at":     testutil.Raw("now() - interval '9 minutes'"),
		"completed_at":   testutil.Raw("now() - interval '1 minute'"),
	}
	for _, o := range over {
		for k, v := range o {
			cols[k] = v
		}
	}
	return dbfx.Task(t, agentID, cols)
}

func stopGateClosedFor(t *testing.T, issueID, wantTaskID string) {
	t.Helper()
	var notConfirmed *service.StopNotConfirmedError
	err := service.StopGate(context.Background(), testHandler.Queries, parseUUID(issueID))
	if !errors.As(err, &notConfirmed) {
		t.Fatalf("StopGate = %v, want *StopNotConfirmedError for run %s", err, wantTaskID)
	}
	if got := uuidToString(notConfirmed.TaskID); got != wantTaskID {
		t.Fatalf("StopGate names run %s, want %s", got, wantTaskID)
	}
}

func stopGateOpen(t *testing.T, issueID string) {
	t.Helper()
	if err := service.StopGate(context.Background(), testHandler.Queries, parseUUID(issueID)); err != nil {
		t.Fatalf("StopGate = %v, want open", err)
	}
}

func cancelAckedAt(t *testing.T, taskID string) (at time.Time, set bool) {
	t.Helper()
	var ts *time.Time
	dbfx.QueryRow(t, `SELECT cancel_acked_at FROM agent_task_queue WHERE id = $1`, taskID).Scan(&ts)
	if ts == nil {
		return time.Time{}, false
	}
	return *ts, true
}

// daemonAck is the ack the real daemon sends: the owner credential plus the
// proof it received in the claim response for this task and runtime.
func daemonAck(t *testing.T, taskID string) {
	t.Helper()
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent_task_queue WHERE id = $1`, taskID).Scan(&runtimeID)
	ackCancelled(t, taskID, map[string]any{"cancel_ack_proof": auth.CancelAckProof(taskID, runtimeID)})
}

func TestStopGate_AckSetsCancelAckedAtAndOpensGate(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := mul8188Agent(t)
	issueID := dbfx.Issue(t, "stop gate ack", testutil.Cols{"status": "todo"})
	run := supersededRun(t, agentID, runtimeID, issueID, "todo")

	stopGateClosedFor(t, issueID, run)
	if _, ok := cancelAckedAt(t, run); ok {
		t.Fatal("cancel_acked_at set before any ack")
	}

	daemonAck(t, run)

	if _, ok := cancelAckedAt(t, run); !ok {
		t.Fatal("ack did not set cancel_acked_at")
	}
	stopGateOpen(t, issueID)
}

// A runtime that stays offline sends no ack. Offline is not proof the runner
// stopped (the daemon ignores network errors while polling), so the gate holds
// beyond any reconnect grace.
func TestStopGate_OfflineBeyondGraceStaysClosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	runtimeID := dbfx.Runtime(t, "stop gate offline", testutil.Cols{
		"status":       "offline",
		"last_seen_at": testutil.Raw("now() - interval '3 hours'"),
	})
	agentID := dbfx.Agent(t, "stop gate offline agent", runtimeID)
	issueID := dbfx.Issue(t, "stop gate offline", testutil.Cols{"status": "todo"})
	run := supersededRun(t, agentID, runtimeID, issueID, "todo")

	stopGateClosedFor(t, issueID, run)
	if _, ok := cancelAckedAt(t, run); ok {
		t.Fatal("cancel_acked_at set without an ack")
	}
}

// The task stays cancelled on the server while the daemon is partitioned. On
// reconnect the daemon sees the terminal status, interrupts the run and acks.
func TestStopGate_PartitionedDaemonReconnectsAcksOpensGate(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := mul8188Agent(t)
	issueID := dbfx.Issue(t, "stop gate partition", testutil.Cols{"status": "todo"})
	run := supersededRun(t, agentID, runtimeID, issueID, "todo")

	stopGateClosedFor(t, issueID, run)
	// The partition lasts: still closed, still the same run.
	stopGateClosedFor(t, issueID, run)

	daemonAck(t, run)

	stopGateOpen(t, issueID)
}

func TestStopGate_ReplayedAckIsIdempotent(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := mul8188Agent(t)
	issueID := dbfx.Issue(t, "stop gate replay", testutil.Cols{"status": "todo"})
	run := supersededRun(t, agentID, runtimeID, issueID, "todo")

	daemonAck(t, run)
	first, ok := cancelAckedAt(t, run)
	if !ok {
		t.Fatal("first ack did not set cancel_acked_at")
	}
	daemonAck(t, run)
	second, _ := cancelAckedAt(t, run)
	if !second.Equal(first) {
		t.Fatalf("replayed ack moved cancel_acked_at from %s to %s", first, second)
	}
	stopGateOpen(t, issueID)
}

// A late ack from a stale run must not stamp a row that was not cancelled.
func TestStopGate_AckIgnoredOnNonCancelledRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := mul8188Agent(t)
	issueID := dbfx.Issue(t, "stop gate cas", testutil.Cols{"status": "todo"})
	completed := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "completed",
		"started_at": testutil.Raw("now()"), "completed_at": testutil.Raw("now()"),
	})

	daemonAck(t, completed)

	if _, ok := cancelAckedAt(t, completed); ok {
		t.Fatal("ack stamped cancel_acked_at on a completed row")
	}
}

// The gate arms only for a superseded run that reached a daemon and sits at the
// issue's current step; everything else stays open.
func TestStopGate_OnlyArmedBySupersededDispatchedRunAtCurrentStep(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := mul8188Agent(t)
	cases := []struct {
		name string
		step string
		over testutil.Cols
	}{
		{"user cancel without a reason", "todo", testutil.Cols{"failure_reason": nil}},
		{"superseded but never dispatched", "todo", testutil.Cols{"dispatched_at": nil}},
		{"superseded at another step", "in_progress", nil},
		{"superseded and already acked", "todo", testutil.Cols{"cancel_acked_at": testutil.Raw("now()")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issueID := dbfx.Issue(t, "stop gate scope", testutil.Cols{"status": "todo"})
			supersededRun(t, agentID, runtimeID, issueID, tc.step, tc.over)
			stopGateOpen(t, issueID)
		})
	}
}

// The review step is closed by an unacked superseded run. A member or an agent
// moving the issue into that step starts nothing, and activity says why.
func TestStopGate_StatusMoveIntoStepRefusedWithActivity(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	assignee := createHandlerTestAgent(t, "Stop Gate Assignee", nil)
	otherAgent := dbfx.Agent(t, "stop gate acting agent", testRuntimeID)
	_, runtimeID := mul8188Agent(t)

	queuedRuns := func(issueID string) (n int) {
		dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, issueID).Scan(&n)
		return n
	}
	newIssue := func(title string) string {
		return dbfx.Issue(t, title, testutil.Cols{
			"status": "backlog", "assignee_type": "agent", "assignee_id": assignee,
		})
	}

	// Control: with the gate open, the same move starts a run.
	control := newIssue("stop gate control")
	testutil.Call(t, testHandler.UpdateIssue, withURLParam(newRequest("PUT", "/api/issues/"+control, map[string]any{"status": "todo"}), "id", control)).Want(http.StatusOK)
	if got := queuedRuns(control); got != 1 {
		t.Fatalf("control: queued runs = %d, want 1", got)
	}

	actors := []struct {
		name, actorType, actorID string
		headers                  map[string]string
	}{
		{"member", "member", testUserID, nil},
		{"agent", "agent", otherAgent, map[string]string{"X-Actor-Source": "task_token", "X-Agent-ID": otherAgent}},
	}
	for _, actor := range actors {
		for _, writer := range issueWriters {
			t.Run(actor.name+"/"+writer.name, func(t *testing.T) {
				issueID := newIssue("stop gate " + actor.name + " " + writer.name)
				run := supersededRun(t, assignee, runtimeID, issueID, "todo")

				writer.write(t, issueID, map[string]any{"status": "todo"}, actor.headers).Want(http.StatusOK)

				wantIssueStatus(t, issueID, "todo")
				if got := queuedRuns(issueID); got != 0 {
					t.Fatalf("queued runs = %d, want 0: the gate must refuse a review run while the old runner is unconfirmed", got)
				}
				var actorType, actorID, message string
				dbfx.QueryRow(t, `
					SELECT actor_type, actor_id::text, details->>'message' FROM activity_log
					WHERE issue_id = $1 AND action = 'stop_not_confirmed'
				`, issueID).Scan(&actorType, &actorID, &message)
				if actorType != actor.actorType || actorID != actor.actorID {
					t.Fatalf("activity actor = %s/%s, want %s/%s", actorType, actorID, actor.actorType, actor.actorID)
				}
				if want := "stop not confirmed for run " + run; !strings.Contains(message, want) {
					t.Fatalf("activity message = %q, want it to contain %q", message, want)
				}
			})
		}
	}
}

// The runtime owner authenticates with the same PAT/JWT as the daemon, so the
// credential alone proves nothing about the runner. An ack without the claim
// proof, or with a proof minted for another run, must leave the gate closed.
func TestStopGate_AckWithoutValidProofDoesNotOpenGate(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := mul8188Agent(t)
	issueID := dbfx.Issue(t, "stop gate no proof", testutil.Cols{"status": "todo"})
	run := supersededRun(t, agentID, runtimeID, issueID, "todo")
	other := supersededRun(t, agentID, runtimeID, dbfx.Issue(t, "stop gate other run", testutil.Cols{"status": "todo"}), "todo")

	bodies := map[string]map[string]any{
		"empty body":          {},
		"empty proof":         {"cancel_ack_proof": ""},
		"garbage proof":       {"cancel_ack_proof": "not-a-proof"},
		"proof for other run": {"cancel_ack_proof": auth.CancelAckProof(other, runtimeID)},
	}
	for name, body := range bodies {
		ackCancelled(t, run, body)
		if _, ok := cancelAckedAt(t, run); ok {
			t.Fatalf("%s: ack without a valid proof set cancel_acked_at", name)
		}
		stopGateClosedFor(t, issueID, run)
	}
}

// An ack that reports the runner's cleanup unconfirmed is not a confirmed
// stop, whatever else it carries — and the FailTask rewrite it triggers
// (cancelled -> failed/cleanup_unconfirmed) must not drop the run out of the
// gate. Only a confirmed stop with the claim proof opens it.
func TestStopGate_UnconfirmedCleanupAckKeepsGateClosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := mul8188Agent(t)
	pending := testutil.Cols{"result": `{"process_cleanup_pending":true}`}
	proof := func(taskID string) string { return auth.CancelAckProof(taskID, runtimeID) }
	cases := map[string]struct {
		over testutil.Cols
		body func(taskID string) map[string]any
	}{
		"no proof, cleanup_unconfirmed reason": {nil, func(string) map[string]any {
			return map[string]any{"failure_reason": "cleanup_unconfirmed", "error_message": "cleanup failed"}
		}},
		"valid proof, cleanup_unconfirmed reason": {nil, func(id string) map[string]any {
			return map[string]any{"cancel_ack_proof": proof(id), "failure_reason": "cleanup_unconfirmed", "error_message": "cleanup failed"}
		}},
		"valid proof, pending cleanup not confirmed": {pending, func(id string) map[string]any {
			return map[string]any{"cancel_ack_proof": proof(id), "process_cleanup_confirmed": false}
		}},
		"valid proof, pending cleanup unreported": {pending, func(id string) map[string]any {
			return map[string]any{"cancel_ack_proof": proof(id)}
		}},
	}
	for name, tc := range cases {
		issueID := dbfx.Issue(t, "stop gate unconfirmed cleanup", testutil.Cols{"status": "todo"})
		run := supersededRun(t, agentID, runtimeID, issueID, "todo", tc.over)
		ackCancelled(t, run, tc.body(run))
		if _, ok := cancelAckedAt(t, run); ok {
			t.Fatalf("%s: unconfirmed cleanup set cancel_acked_at", name)
		}
		stopGateClosedFor(t, issueID, run)
	}

	// Control: the same pending run opens the gate once cleanup is confirmed.
	issueID := dbfx.Issue(t, "stop gate confirmed cleanup", testutil.Cols{"status": "todo"})
	run := supersededRun(t, agentID, runtimeID, issueID, "todo", pending)
	ackCancelled(t, run, map[string]any{"cancel_ack_proof": proof(run), "process_cleanup_confirmed": true})
	stopGateOpen(t, issueID)
}

// Every terminal transition of an armed run keeps it owed a stop confirmation,
// not only /cancel-ack: the owner credential reaches /fail too, and the row's
// status or failure_reason is rewritten by many writers. The guard lives with
// the row, so a writer added later inherits it.
func TestStopGate_TerminalTransitionsKeepArmedRunClosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := mul8188Agent(t)
	fail := func(body map[string]any) func(t *testing.T, id string) {
		return func(t *testing.T, id string) {
			req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/tasks/"+id+"/fail", body)
			testutil.Call(t, testHandler.FailTask, testutil.WithURLParams(req, "taskId", id)).Want(http.StatusOK)
		}
	}
	cases := map[string]func(t *testing.T, id string){
		"/fail cleanup_unconfirmed": fail(map[string]any{"error": "cleanup failed", "failure_reason": "cleanup_unconfirmed"}),
		"/fail plain error":         fail(map[string]any{"error": "boom"}),
		"status rewritten directly": func(t *testing.T, id string) {
			dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'failed', failure_reason = 'agent_error' WHERE id = $1`, id)
		},
		"failure_reason rewritten directly": func(t *testing.T, id string) {
			dbfx.Exec(t, `UPDATE agent_task_queue SET failure_reason = 'cleanup_unconfirmed' WHERE id = $1`, id)
		},
	}
	for name, transition := range cases {
		issueID := dbfx.Issue(t, "stop gate terminal transition", testutil.Cols{"status": "todo"})
		run := supersededRun(t, agentID, runtimeID, issueID, "todo")
		transition(t, run)
		if _, ok := cancelAckedAt(t, run); ok {
			t.Fatalf("%s: set cancel_acked_at", name)
		}
		stopGateClosedFor(t, issueID, run)
	}

	// Control: a proof-bearing ack still opens the gate, and a later rewrite
	// of the acked row does not close it again.
	issueID := dbfx.Issue(t, "stop gate acked then rewritten", testutil.Cols{"status": "todo"})
	run := supersededRun(t, agentID, runtimeID, issueID, "todo")
	daemonAck(t, run)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'failed', failure_reason = 'agent_error' WHERE id = $1`, run)
	stopGateOpen(t, issueID)
}
