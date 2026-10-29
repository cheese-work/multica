package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// State table for the stop gate (CHE-2309 round 5). Rows are the states an
// armed run goes through, columns are the events that reach it; every cell is
// one case below. The gate opens only on a verified, confirmed ack — whether
// the run is still `cancelled` or was rewritten first (`failed`, `completed`)
// by an unconfirmed ack, /fail or a late /complete.
//
//	state \ event            confirmed ack | unconfirmed ack | ack, no proof | /fail | late /complete | replayed ack
//	cancelled (armed)        open          | closed          | closed        | closed| closed         | n/a
//	failed, retained         open          | closed          | closed        | closed| closed         | n/a
//	completed, retained      open          | closed          | closed        | closed| closed         | n/a
//	acked (settled)          open          | open            | open          | open  | open           | open, ack time unchanged
//	failed, not armed        no-op         | no-op           | no-op         | -     | -              | -

type stopEvent func(t *testing.T, id, runtimeID string)

func ackEvent(body func(id, runtimeID string) map[string]any) stopEvent {
	return func(t *testing.T, id, runtimeID string) {
		t.Helper()
		ackCancelled(t, id, body(id, runtimeID))
	}
}

var (
	confirmedAck = ackEvent(func(id, rt string) map[string]any {
		return map[string]any{"cancel_ack_proof": auth.CancelAckProof(id, rt), "process_cleanup_confirmed": true}
	})
	unconfirmedAck = ackEvent(func(id, rt string) map[string]any {
		return map[string]any{"cancel_ack_proof": auth.CancelAckProof(id, rt), "failure_reason": "cleanup_unconfirmed", "error_message": "cleanup failed"}
	})
	// Proof present, no statement about cleanup: not a confirmation once the
	// run has reported its cleanup unconfirmed.
	silentAck = ackEvent(func(id, rt string) map[string]any {
		return map[string]any{"cancel_ack_proof": auth.CancelAckProof(id, rt)}
	})
	ackNoProof = ackEvent(func(string, string) map[string]any {
		return map[string]any{"process_cleanup_confirmed": true}
	})
	failCallback stopEvent = func(t *testing.T, id, _ string) {
		t.Helper()
		req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/tasks/"+id+"/fail", map[string]any{"error": "boom"})
		testutil.Call(t, testHandler.FailTask, testutil.WithURLParams(req, "taskId", id))
	}
	lateComplete stopEvent = func(t *testing.T, id, _ string) {
		t.Helper()
		req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/tasks/"+id+"/complete", map[string]any{"output": "done"})
		testutil.Call(t, testHandler.CompleteTask, testutil.WithURLParams(req, "taskId", id))
	}
	// Writers that rewrite the row without the daemon: the guard is the row's.
	rewriteCompleted stopEvent = func(t *testing.T, id, _ string) {
		t.Helper()
		dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', failure_reason = NULL WHERE id = $1`, id)
	}
)

func TestStopGate_StateTable(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := mul8188Agent(t)
	cases := []struct {
		name     string
		prior    []stopEvent // events that happened before the final one
		final    stopEvent
		wantOpen bool
	}{
		// cancelled (armed)
		{"cancelled / confirmed ack", nil, confirmedAck, true},
		{"cancelled / unconfirmed ack", nil, unconfirmedAck, false},
		{"cancelled / ack without proof", nil, ackNoProof, false},
		{"cancelled / fail callback", nil, failCallback, false},
		{"cancelled / late complete", nil, lateComplete, false},
		// failed, retained (armed row rewritten before any confirmation)
		{"failed / confirmed ack", []stopEvent{failCallback}, confirmedAck, true},
		{"failed after unconfirmed ack / confirmed ack", []stopEvent{unconfirmedAck}, confirmedAck, true},
		{"failed / unconfirmed ack", []stopEvent{unconfirmedAck}, unconfirmedAck, false},
		{"failed / silent ack", []stopEvent{unconfirmedAck}, silentAck, false},
		{"failed / ack without proof", []stopEvent{failCallback}, ackNoProof, false},
		{"failed / fail callback", []stopEvent{failCallback}, failCallback, false},
		{"failed / late complete", []stopEvent{failCallback}, lateComplete, false},
		// completed, retained
		{"completed / confirmed ack", []stopEvent{rewriteCompleted}, confirmedAck, true},
		{"completed / unconfirmed ack", []stopEvent{rewriteCompleted}, unconfirmedAck, false},
		{"completed / ack without proof", []stopEvent{rewriteCompleted}, ackNoProof, false},
		// acked (settled): nothing closes it again
		{"acked / unconfirmed ack", []stopEvent{confirmedAck}, unconfirmedAck, true},
		{"acked / fail callback", []stopEvent{confirmedAck}, failCallback, true},
		{"acked / late complete", []stopEvent{confirmedAck}, lateComplete, true},
		{"acked / rewritten", []stopEvent{confirmedAck}, rewriteCompleted, true},
	}
	for _, tc := range cases {
		issueID := dbfx.Issue(t, "stop gate state table", testutil.Cols{"status": "todo"})
		run := supersededRun(t, agentID, runtimeID, issueID, "todo")
		for _, e := range tc.prior {
			e(t, run, runtimeID)
		}
		tc.final(t, run, runtimeID)
		if tc.wantOpen {
			if err := stopGateErr(issueID); err != nil {
				t.Errorf("%s: gate closed (%v), want open", tc.name, err)
			}
		} else if err := stopGateErr(issueID); err == nil {
			t.Errorf("%s: gate open, want closed", tc.name)
		}
	}
}

// A replayed ack changes nothing: the first ack time wins.
func TestStopGate_ReplayedConfirmedAckKeepsFirstAckTime(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := mul8188Agent(t)
	issueID := dbfx.Issue(t, "stop gate replay", testutil.Cols{"status": "todo"})
	run := supersededRun(t, agentID, runtimeID, issueID, "todo")
	confirmedAck(t, run, runtimeID)
	first, ok := cancelAckedAt(t, run)
	if !ok {
		t.Fatal("confirmed ack did not set cancel_acked_at")
	}
	confirmedAck(t, run, runtimeID)
	if again, _ := cancelAckedAt(t, run); !again.Equal(first) {
		t.Fatalf("replayed ack moved cancel_acked_at from %v to %v", first, again)
	}
}

// A confirmed ack settles only a run the gate owes a stop: an ordinary failed
// row with no armed marker is left alone.
func TestStopGate_ConfirmedAckDoesNotSettleUnrelatedFailedRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := mul8188Agent(t)
	issueID := dbfx.Issue(t, "stop gate unrelated failed", testutil.Cols{"status": "todo"})
	for name, over := range map[string]testutil.Cols{
		"failed, other reason":        {"status": "failed", "failure_reason": "agent_error"},
		"failed, cleanup_unconfirmed": {"status": "failed", "failure_reason": "cleanup_unconfirmed"},
		"completed":                   {"status": "completed", "failure_reason": nil},
	} {
		run := supersededRun(t, agentID, runtimeID, issueID, "todo", over)
		confirmedAck(t, run, runtimeID)
		if _, set := cancelAckedAt(t, run); set {
			t.Errorf("%s: confirmed ack stamped cancel_acked_at on an unrelated row", name)
		}
	}
}

func stopGateErr(issueID string) error {
	return service.StopGate(context.Background(), testHandler.Queries, parseUUID(issueID))
}
