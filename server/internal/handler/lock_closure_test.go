package handler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// queryHook runs fn once, right after the nth statement whose text holds marker
// has returned its rows (nth counts from 1). That is the window between a
// closure's discovery and its lock: a run created in it is what the closure has
// to catch.
type queryHook struct {
	marker string
	nth    int
	seen   int
	fn     func()
}

type afterQueryTx struct {
	db.DBTX
	hooks []*queryHook
}

func (c *afterQueryTx) Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	rows, err := c.DBTX.Query(ctx, sql, args...)
	if err != nil {
		return rows, err
	}
	for _, h := range c.hooks {
		if strings.Contains(sql, h.marker) {
			h.seen++
			if h.seen == h.nth {
				h.fn()
			}
		}
	}
	return rows, err
}

// issueLockedByAnotherTx reports whether a second connection cannot take the
// issue row, i.e. whether the open transaction under test holds it.
func issueLockedByAnotherTx(t *testing.T, issueID string) bool {
	t.Helper()
	ctx := context.Background()
	probe, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin probe: %v", err)
	}
	defer probe.Rollback(ctx)
	if _, err := probe.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
		t.Fatalf("set probe lock_timeout: %v", err)
	}
	_, err = probe.Exec(ctx, `SELECT 1 FROM issue WHERE id = $1 FOR UPDATE`, issueID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		return true
	}
	if err != nil {
		t.Fatalf("probe issue lock: %v", err)
	}
	return false
}

func newForeignIssue(t *testing.T, workspaceID, title string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `
INSERT INTO issue (workspace_id, title, creator_type, creator_id, number)
VALUES ($1, $2, 'member', $3,
        (SELECT COALESCE(max(number), 0) + 1 FROM issue WHERE workspace_id = $1))
RETURNING id`, workspaceID, title, testUserID).Scan(&id); err != nil {
		t.Fatalf("create issue %q: %v", title, err)
	}
	return id
}

func newDeletePathComment(t *testing.T, workspaceID, issueID string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `
INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content)
VALUES ($1, $2, 'member', $3, 'comment')
RETURNING id`, workspaceID, issueID, testUserID).Scan(&id); err != nil {
		t.Fatalf("create comment: %v", err)
	}
	return id
}

// A run on another issue that names the comment after the closure read its
// issues must still end up locked, and the comment row must be held so that no
// further run can name it.
func TestLockCommentDeleteClosureCatchesALateCommentReference(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	f := newWorkspaceDeletePathFixture(t, "closure-late-comment")
	commentID := newDeletePathComment(t, f.victimID, f.victimIssue)
	lateIssue := newForeignIssue(t, f.neighbourID, "late comment reference")

	tx, err := testHandler.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	w := &afterQueryTx{DBTX: tx, hooks: []*queryHook{{marker: "ListIssuesOfCommentDelete", nth: 1, fn: func() {
		if _, err := testPool.Exec(ctx, `
INSERT INTO agent_task_queue (agent_id, issue_id, runtime_id, status, completed_at, trigger_comment_id)
VALUES ($1, $2, $3, 'completed', now(), $4)`, f.neighbourAgent, lateIssue, f.neighbourRuntime, commentID); err != nil {
			t.Fatalf("create late run: %v", err)
		}
	}}}}

	if err := lockCommentDeleteClosure(ctx, db.New(w), parseUUID(commentID), parseUUID(f.victimID)); err != nil {
		t.Fatalf("closure: %v", err)
	}
	if !issueLockedByAnotherTx(t, lateIssue) {
		t.Fatal("the issue of a run that named the comment after discovery is not locked")
	}

	// The fence: a run that starts to name the comment now waits on its row lock.
	probe, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin probe: %v", err)
	}
	defer probe.Rollback(ctx)
	if _, err := probe.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
		t.Fatalf("set probe lock_timeout: %v", err)
	}
	_, err = probe.Exec(ctx, `
INSERT INTO agent_task_queue (agent_id, issue_id, runtime_id, status, completed_at, trigger_comment_id)
VALUES ($1, $2, $3, 'completed', now(), $4)`, f.neighbourAgent, f.neighbourIssue, f.neighbourRuntime, commentID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("a run named the comment while the closure held it: err = %v, want lock_not_available", err)
	}
}

// A run created as the child of a run of the deleted issue, on a third issue,
// after the closure read the issues, must also be locked.
func TestLockIssueDeleteClosureCatchesALateChild(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	f := newWorkspaceDeletePathFixture(t, "closure-late-child")
	parent := insertDeletePathTask(t, f.victimAgent, f.victimIssue, f.victimRuntime)
	lateIssue := newForeignIssue(t, f.neighbourID, "late child")

	tx, err := testHandler.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	w := &afterQueryTx{DBTX: tx, hooks: []*queryHook{{marker: "ListIssuesOfIssueDelete", nth: 1, fn: func() {
		if _, err := testPool.Exec(ctx, `
INSERT INTO agent_task_queue (agent_id, issue_id, runtime_id, status, completed_at, parent_task_id)
VALUES ($1, $2, $3, 'completed', now(), $4)`, f.neighbourAgent, lateIssue, f.neighbourRuntime, parent); err != nil {
			t.Fatalf("create late child: %v", err)
		}
	}}}}

	if err := lockIssueDeleteClosure(ctx, db.New(w), []pgtype.UUID{parseUUID(f.victimIssue)}); err != nil {
		t.Fatalf("closure: %v", err)
	}
	if !issueLockedByAnotherTx(t, lateIssue) {
		t.Fatal("the issue of a child created after discovery is not locked")
	}
}

// The same for deleting an agent, through the production deleteSystemAgent.
func TestDeleteSystemAgentLocksTheIssueOfALateChild(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	f := newWorkspaceDeletePathFixture(t, "closure-late-agent-child")
	parent := insertDeletePathTask(t, f.victimAgent, f.victimIssue, f.victimRuntime)
	lateIssue := newForeignIssue(t, f.neighbourID, "late agent child")

	tx, err := testHandler.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	w := &afterQueryTx{DBTX: tx, hooks: []*queryHook{{marker: "ListIssuesOfAgentTaskChildren", nth: 1, fn: func() {
		if _, err := testPool.Exec(ctx, `
INSERT INTO agent_task_queue (agent_id, issue_id, runtime_id, status, completed_at, parent_task_id)
VALUES ($1, $2, $3, 'completed', now(), $4)`, f.neighbourAgent, lateIssue, f.neighbourRuntime, parent); err != nil {
			t.Fatalf("create late child: %v", err)
		}
	}}}}

	if err := deleteSystemAgent(ctx, db.New(w), parseUUID(f.victimAgent)); err != nil {
		t.Fatalf("delete system agent: %v", err)
	}
	if !issueLockedByAnotherTx(t, lateIssue) {
		t.Fatal("the issue of a child created after discovery is not locked")
	}
}

// The third-issue crossing. A concurrent AuthorizeElevatedOp holds the issue of a
// child that appeared during the page and then waits on the parent's run row. A
// page that holds that run row while it asks for the issue closes a cycle; the
// page has to give the run row up, lock the larger issue set, and take the row
// again.
func TestLockTaskPageIssuesFirstGivesUpTheRunRowsBeforeWaitingForALateIssue(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	f := newWorkspaceDeletePathFixture(t, "page-third-issue")
	parent := insertDeletePathTask(t, f.victimAgent, f.victimIssue, f.victimRuntime)
	lateIssue := newForeignIssue(t, f.neighbourID, "late issue held by an authorizer")

	auth, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin authorizer: %v", err)
	}
	defer auth.Rollback(ctx)
	authErr := make(chan error, 1)
	waitUntilAuthorizerWaits := func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var waiting bool
			if err := testPool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_stat_activity
               WHERE wait_event_type = 'Lock'
                 AND query LIKE '%FROM agent_task_queue WHERE id = $1 FOR UPDATE%'
                 AND query NOT LIKE '%pg_stat_activity%')`).Scan(&waiting); err == nil && waiting {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Error("the authorizer never waited on the run row")
	}

	tx, err := testHandler.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '10s'`); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}
	w := &afterQueryTx{DBTX: tx, hooks: []*queryHook{
		// After the page's first read: a child on the late issue appears, and an
		// authorizer takes the late issue.
		{marker: "ORDER BY t.issue_id", nth: 1, fn: func() {
			if _, err := testPool.Exec(ctx, `
INSERT INTO agent_task_queue (agent_id, issue_id, runtime_id, status, completed_at, parent_task_id)
VALUES ($1, $2, $3, 'completed', now(), $4)`, f.neighbourAgent, lateIssue, f.neighbourRuntime, parent); err != nil {
				t.Fatalf("create child run: %v", err)
			}
			if _, err := auth.Exec(ctx, `SELECT 1 FROM issue WHERE id = $1 FOR UPDATE`, lateIssue); err != nil {
				t.Fatalf("authorizer locks issue: %v", err)
			}
		}},
		// After the read that follows the run-row lock: the authorizer asks for the
		// parent's run row, and waits there while the page holds it.
		{marker: "ORDER BY t.issue_id", nth: 2, fn: func() {
			go func() {
				_, err := auth.Exec(ctx, `SELECT 1 FROM agent_task_queue WHERE id = $1 FOR UPDATE`, parent)
				if err == nil {
					err = auth.Commit(ctx)
				}
				authErr <- err
			}()
			waitUntilAuthorizerWaits()
		}},
	}}

	locked, err := lockTaskPageIssuesFirst(ctx, db.New(w), parseUUID(f.victimAgent), []pgtype.UUID{parseUUID(parent)})
	if err != nil {
		t.Fatalf("lock page: %v", err)
	}
	if len(locked) != 1 {
		t.Fatalf("locked %d task rows, want 1", len(locked))
	}
	select {
	case err := <-authErr:
		if err != nil {
			t.Fatalf("authorizer: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the authorizer never finished")
	}
}
