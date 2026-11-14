package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
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

// seedBatchRow inserts a run_input row as a slice 1 broad claim or a retry chain
// would have left it.
func (e *admEnv) seedBatchRow(issueID, event, state string, task pgtype.UUID, reason string, thread pgtype.UUID) int64 {
	e.t.Helper()
	var seq int64
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO run_input (workspace_id, issue_id, agent_id, event_id, kind, payload, state, task_id, park_reason, thread_key)
		VALUES ($1, $2, $3, $4, 'comment', '{}', $5, $6, NULLIF($7, ''), $8) RETURNING seq`,
		e.ws, issueID, e.agent, event, state, task, reason, thread).Scan(&seq); err != nil {
		e.t.Fatalf("seed %s: %v", event, err)
	}
	return seq
}

func (e *admEnv) rowState(seq int64) (state string, owner pgtype.UUID, reason pgtype.Text) {
	e.t.Helper()
	if err := e.pool.QueryRow(context.Background(), `SELECT state, owner_task_id, park_reason FROM run_input WHERE seq = $1`, seq).Scan(&state, &owner, &reason); err != nil {
		e.t.Fatalf("read row %d: %v", seq, err)
	}
	return
}

func retryPair(issueID string, thread pgtype.UUID) (parent, child db.AgentTaskQueue) {
	iid := util.MustParseUUID(issueID)
	parent = db.AgentTaskQueue{ID: dbid.NewV7(), IssueID: iid, CommentThreadID: thread}
	child = db.AgentTaskQueue{ID: dbid.NewV7(), IssueID: iid, CommentThreadID: thread}
	return
}

// 14i, 14j: a quarantined or legacy-parked row of the failed run's batch is
// never re-held to the retry child; the run's valid row is.
func TestRunInputRetryNeverReheldsQuarantinedRows(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	ctx := context.Background()
	thread := e.threadComment(issue)
	parent, child := retryPair(issue, thread)
	good := e.seedBatchRow(issue, "good", "claimed", parent.ID, "", thread)
	quarantined := e.seedBatchRow(issue, "quarantined", "parked", parent.ID, "legacy_claim_unproven", thread)
	legacy := e.seedBatchRow(issue, "legacy", "parked", parent.ID, "legacy_unowned", thread)

	if err := redeliverToRetry(ctx, e.svc.Queries, parent, child); err != nil {
		t.Fatalf("redeliver: %v", err)
	}
	if st, owner, _ := e.rowState(good); st != "owned" || owner != child.ID {
		t.Fatalf("valid row: state %q owner %v, want owned by the retry child", st, owner)
	}
	for name, seq := range map[string]int64{"legacy_claim_unproven": quarantined, "legacy_unowned": legacy} {
		if st, owner, reason := e.rowState(seq); st != "parked" || owner.Valid || reason.String != name {
			t.Fatalf("%s row: state %q owner %v reason %q, want still parked with its reason", name, st, owner, reason.String)
		}
	}
}

// 14k: a valid retry chain still re-holds, and a row a member resumed and that
// failed again carries no stale reason that blocks it.
func TestRunInputRetryChainRehelds(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	ctx := context.Background()
	thread := e.threadComment(issue)
	parent, child := retryPair(issue, thread)
	seq := e.seedBatchRow(issue, "x1", "claimed", parent.ID, "", thread)
	if err := redeliverToRetry(ctx, e.svc.Queries, parent, child); err != nil {
		t.Fatal(err)
	}
	if st, owner, reason := e.rowState(seq); st != "owned" || owner != child.ID || reason.Valid {
		t.Fatalf("first retry: state %q owner %v reason %v", st, owner, reason)
	}
	// The child claims the row, fails, and its own retry follows.
	if _, err := e.pool.Exec(ctx, `UPDATE run_input SET state = 'claimed', task_id = $2 WHERE seq = $1`, seq, child.ID); err != nil {
		t.Fatal(err)
	}
	_, next := retryPair(issue, thread)
	if err := redeliverToRetry(ctx, e.svc.Queries, child, next); err != nil {
		t.Fatal(err)
	}
	if st, owner, _ := e.rowState(seq); st != "owned" || owner != next.ID {
		t.Fatalf("second retry: state %q owner %v, want owned by the next child", st, owner)
	}
}

// Resume works per thread: a parked row of a known thread goes to a run of that
// thread, a row of unknown thread stays parked and is counted.
func TestRunInputResumePerThreadSkipsUnknownThread(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	ctx := context.Background()
	thread := e.threadComment(issue)
	known := e.seedBatchRow(issue, "known", "parked", pgtype.UUID{}, "failed", thread)
	unknown := e.seedBatchRow(issue, "unknown", "parked", pgtype.UUID{}, "ambiguous_owner", pgtype.UUID{})

	res, err := e.svc.ResumeParkedInputs(ctx, util.MustParseUUID(issue), util.MustParseUUID(e.agent))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Resumed != 1 || res.SkippedUnknownThread != 1 {
		t.Fatalf("result %+v, want 1 resumed and 1 skipped for unknown thread", res)
	}
	st, owner, _ := e.rowState(known)
	if st != "owned" || !owner.Valid {
		t.Fatalf("known-thread row: state %q owner %v, want owned by a new carrier", st, owner)
	}
	var carrierThread pgtype.UUID
	if err := e.pool.QueryRow(ctx, `SELECT comment_thread_id FROM agent_task_queue WHERE id = $1`, owner).Scan(&carrierThread); err != nil || carrierThread != thread {
		t.Fatalf("carrier thread %v (err %v), want %v", carrierThread, err, thread)
	}
	if st, owner, _ := e.rowState(unknown); st != "parked" || owner.Valid {
		t.Fatalf("unknown-thread row: state %q owner %v, want still parked", st, owner)
	}
}
