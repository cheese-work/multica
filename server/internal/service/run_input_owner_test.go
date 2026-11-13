package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// threadComment inserts a top-level comment (its own thread root) on the issue.
func (e *admEnv) threadComment(issueID string) pgtype.UUID {
	e.t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
		VALUES ($1, $2, 'member', $3, 'thread root', 'comment') RETURNING id`, issueID, e.ws, e.user).Scan(&id); err != nil {
		e.t.Fatalf("seed comment: %v", err)
	}
	return util.MustParseUUID(id)
}

// carrierInThread queues a run of the issue's assignee in the thread of a new comment.
func (e *admEnv) carrierInThread(issueID string) (db.AgentTaskQueue, pgtype.UUID) {
	e.t.Helper()
	issue, err := e.svc.Queries.GetIssue(context.Background(), util.MustParseUUID(issueID))
	if err != nil {
		e.t.Fatalf("get issue: %v", err)
	}
	c := e.threadComment(issueID)
	task, err := e.svc.EnqueueTaskForIssue(context.Background(), issue, c)
	if err != nil {
		e.t.Fatalf("enqueue in thread: %v", err)
	}
	return task, c
}

// inputOf returns the state and task id of the input recorded for a carrier.
func (e *admEnv) inputOf(issueID string, carrier pgtype.UUID) (state string, task pgtype.UUID) {
	e.t.Helper()
	if err := e.pool.QueryRow(context.Background(),
		`SELECT state, task_id FROM run_input WHERE issue_id = $1 AND payload->>'task_id' = $2`,
		issueID, util.UUIDToString(carrier)).Scan(&state, &task); err != nil {
		e.t.Fatalf("input of carrier %s: %v", util.UUIDToString(carrier), err)
	}
	return state, task
}

// Test 1 of the slice 2 design: a run of one thread that ends before any claim
// must not park the input another carrier of the same (issue, agent) is holding.
func TestRunInputOwnerCancelledCarrierKeepsOtherThreadInput(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	x, _ := e.carrierInThread(issue)
	y, _ := e.carrierInThread(issue)
	if _, err := e.svc.CancelTask(context.Background(), x.ID); err != nil {
		t.Fatalf("cancel X: %v", err)
	}
	if st, _ := e.inputOf(issue, y.ID); st == "parked" {
		t.Fatalf("input of Y is %q after X was cancelled: it belongs to Y and must stay deliverable", st)
	}
}

// Test 2: two carriers of one (issue, agent) on different thread keys. A claim
// binds only the input its own run owns.
func TestRunInputOwnerClaimBindsOnlyOwnInput(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	x, _ := e.carrierInThread(issue)
	y, _ := e.carrierInThread(issue)
	first := e.claim()
	if first == nil {
		t.Fatal("no run claimed")
	}
	other := y.ID
	if first.ID == y.ID {
		other = x.ID
	}
	if st, _ := e.inputOf(issue, first.ID); st != "claimed" {
		t.Fatalf("input of the claimed run = %q, want claimed", st)
	}
	if st, tid := e.inputOf(issue, other); st == "claimed" || tid.Valid {
		t.Fatalf("the other carrier's input is %q bound to %v: a claim bound an input it does not own", st, tid)
	}
}
