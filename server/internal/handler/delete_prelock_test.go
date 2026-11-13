package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multica-ai/multica/server/internal/service"
)

// The issue, comment, agent and runtime deletes remove rows through legacy
// ON DELETE CASCADE / SET NULL foreign keys. Each removed run or comment nulls a
// reference on a run that can belong to a third issue, and that null-update
// runs the run's taint trigger, which locks that third issue. The delete
// therefore reads the third issues, locks them, and only then deletes.
//
// Under READ COMMITTED the read is stale by the time the delete runs: a run
// created in between names a third issue nobody locked. These tests create
// that run through the production path, between the pre-lock read and the first
// delete statement, and check what the delete holds.

// prelockStarter wraps every transaction the handler begins. When a statement
// of the pre-lock first runs, it creates a late reference with a separate
// connection. Before the first statement after the pre-lock, it records
// whether the late run's issue is held by the delete and then runs onProbe.
type prelockStarter struct {
	t     *testing.T
	inner txStarter

	// listMarker names the pre-lock statement; the old :exec lock query and the
	// new :many list query both contain it.
	listMarker string
	// probeMarker names the first statement after the pre-lock.
	probeMarker string
	// late creates the late reference and returns the late run's issue.
	late func() string
	// onProbe runs once, before probeMarker, with the delete's locks held.
	onProbe func()
	// taskRow, when set, must not be locked by the delete when the pre-lock
	// starts: the pre-lock may not run while the delete holds task rows.
	taskRow string

	// lateEveryAttempt makes late run after the first read of every pre-lock
	// attempt, not only the first attempt, so the set never settles.
	lateEveryAttempt bool

	listSeen, probed  bool
	matches           int
	lateIssue         string
	lateIssues        []string
	lateHeldAtProbe   bool
	taskHeldAtList    bool
	savepointRollback int
}

type prelockTx struct {
	pgx.Tx
	s *prelockStarter
}

func (s *prelockStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.inner.Begin(ctx)
	if err != nil {
		return tx, err
	}
	return &prelockTx{Tx: tx, s: s}, nil
}

func (p *prelockTx) before(sql string) {
	s := p.s
	if strings.Contains(sql, "ROLLBACK TO SAVEPOINT") {
		s.savepointRollback++
	}
	if !s.listSeen && strings.Contains(sql, s.listMarker) && s.taskRow != "" {
		s.taskHeldAtList = !rowLockableNow(s.t, "agent_task_queue", s.taskRow)
	}
	if s.listSeen && !s.probed && s.probeMarker != "" && strings.Contains(sql, s.probeMarker) {
		s.probed = true
		if s.lateIssue != "" {
			s.lateHeldAtProbe = !issueLockableNow(s.t, s.lateIssue)
		}
		if s.onProbe != nil {
			s.onProbe()
		}
	}
}

func (p *prelockTx) after(sql string) {
	s := p.s
	if strings.Contains(sql, s.listMarker) {
		s.matches++
		first := !s.listSeen
		s.listSeen = true
		// The first read of an attempt is the odd one: the second read follows
		// the locks and the freeze, after which no late reference can land.
		if s.late != nil && (first || (s.lateEveryAttempt && s.matches%2 == 1)) {
			s.lateIssue = s.late()
			s.lateIssues = append(s.lateIssues, s.lateIssue)
		}
	}
}

func (p *prelockTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	p.before(sql)
	tag, err := p.Tx.Exec(ctx, sql, args...)
	if err == nil {
		p.after(sql)
	}
	return tag, err
}

func (p *prelockTx) Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	p.before(sql)
	rows, err := p.Tx.Query(ctx, sql, args...)
	if err == nil {
		p.after(sql)
	}
	return rows, err
}

func (p *prelockTx) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	p.before(sql)
	row := p.Tx.QueryRow(ctx, sql, args...)
	p.after(sql)
	return row
}

func withPrelockStarter(s *prelockStarter, fn func()) {
	orig := testHandler.TxStarter
	testHandler.TxStarter = s
	defer func() { testHandler.TxStarter = orig }()
	fn()
}

// rowLockableNow reports whether another session can lock the row right now.
func rowLockableNow(t *testing.T, table, id string) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin probe: %v", err)
	}
	defer tx.Rollback(ctx)
	var one int
	err = tx.QueryRow(ctx, `SELECT 1 FROM `+table+` WHERE id = $1 FOR UPDATE NOWAIT`, id).Scan(&one)
	if err == nil {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		return false
	}
	t.Fatalf("probe %s row lock: %v", table, err)
	return false
}

func insertLateIssue(t *testing.T, workspaceID string) string {
	t.Helper()
	var issueID string
	if err := testPool.QueryRow(context.Background(), `
INSERT INTO issue (workspace_id, title, creator_type, creator_id, number)
VALUES ($1, 'late third issue', 'member', $2,
        (SELECT COALESCE(max(number), 0) + 1 FROM issue WHERE workspace_id = $1))
RETURNING id`, workspaceID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("create late issue: %v", err)
	}
	return issueID
}

// lateRun is a run created after the pre-lock read. Its issue is a third issue
// of another workspace and its owners are that workspace's, so owner-row locks
// of the delete do not hold it back.
type lateRun struct {
	workspaceID, agentID, runtimeID string
	parent, comment                 string
}

// insert creates the run on a new third issue. With lockTimeout it gives up on a
// lock wait instead of blocking, and it returns the error instead of failing.
func (l lateRun) insert(t *testing.T, issueID string, lockTimeout bool) error {
	t.Helper()
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin late run: %v", err)
	}
	defer tx.Rollback(ctx)
	if lockTimeout {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '400ms'`); err != nil {
			t.Fatalf("set lock timeout: %v", err)
		}
	}
	var parent, comment any
	if l.parent != "" {
		parent = l.parent
	}
	if l.comment != "" {
		comment = l.comment
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO agent_task_queue (agent_id, issue_id, runtime_id, status, completed_at, parent_task_id, trigger_comment_id)
VALUES ($1, $2, $3, 'completed', now(), $4, $5)`, l.agentID, issueID, l.runtimeID, parent, comment); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (l lateRun) create(t *testing.T) string {
	t.Helper()
	issueID := insertLateIssue(t, l.workspaceID)
	if err := l.insert(t, issueID, false); err != nil {
		t.Fatalf("create late run: %v", err)
	}
	return issueID
}

func requireLockTimeout(t *testing.T, err error, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("%s: want a lock timeout (55P03) because the delete holds the referenced row, got %v", what, err)
	}
}

func requireLateIssueHeld(t *testing.T, s *prelockStarter) {
	t.Helper()
	if !s.listSeen {
		t.Fatalf("the pre-lock never ran")
	}
	if s.lateIssue == "" {
		t.Fatalf("the late run was never created")
	}
	if !s.probed {
		t.Fatalf("the statement after the pre-lock (%s) never ran", s.probeMarker)
	}
	if !s.lateHeldAtProbe {
		t.Fatalf("the delete reached %s without holding the issue of a run created after the pre-lock read: "+
			"its null-update locks that issue while the delete holds run rows, against an authorization that "+
			"holds the issue and waits on the run row (40P01)", s.probeMarker)
	}
}

func deleteVictimIssue(t *testing.T, issueID string) error {
	t.Helper()
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	_, err = testHandler.deleteIssueAndCollectAttachmentURLs(context.Background(), issue, nil)
	return err
}

// A child run created after the pre-lock read names a third issue.
func TestIssueDeleteLocksTheIssueOfAChildCreatedAfterThePrelockRead(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	f := newWorkspaceDeletePathFixture(t, "prelock-issue-child")
	parent := insertDeletePathTask(t, f.victimAgent, f.victimIssue, f.victimRuntime)
	late := lateRun{workspaceID: f.neighbourID, agentID: f.neighbourAgent, runtimeID: f.neighbourRuntime, parent: parent}
	s := &prelockStarter{t: t, inner: testHandler.TxStarter, listMarker: "IssuesOfIssueDelete",
		probeMarker: "name: LockIssueForDelete", late: func() string { return late.create(t) }}

	withPrelockStarter(s, func() {
		if err := deleteVictimIssue(t, f.victimIssue); err != nil {
			t.Fatalf("delete issue: %v", err)
		}
	})
	requireLateIssueHeld(t, s)
	if s.savepointRollback != 1 {
		t.Fatalf("pre-lock restarts = %d, want 1: the issue found after the first read must be locked by a restart, not on top of the locks already held", s.savepointRollback)
	}
	if rowExists(t, "issue", f.victimIssue) {
		t.Fatalf("the victim issue survived its delete")
	}
}

// When a late reference lands in every attempt, the delete gives up instead of
// retrying forever, and it holds nothing afterwards.
func TestIssueDeleteFailsClosedWhenTheIssueSetKeepsWidening(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	f := newWorkspaceDeletePathFixture(t, "prelock-issue-endless")
	parent := insertDeletePathTask(t, f.victimAgent, f.victimIssue, f.victimRuntime)
	late := lateRun{workspaceID: f.neighbourID, agentID: f.neighbourAgent, runtimeID: f.neighbourRuntime, parent: parent}
	s := &prelockStarter{t: t, inner: testHandler.TxStarter, listMarker: "IssuesOfIssueDelete",
		lateEveryAttempt: true, late: func() string { return late.create(t) }}

	var err error
	withPrelockStarter(s, func() { err = deleteVictimIssue(t, f.victimIssue) })
	if !errors.Is(err, service.ErrPrelockUnstable) {
		t.Fatalf("delete error = %v, want service.ErrPrelockUnstable", err)
	}
	if want := service.MaxPrelockRestarts + 1; len(s.lateIssues) != want {
		t.Fatalf("attempts = %d, want %d (the first run plus %d restarts)", len(s.lateIssues), want, service.MaxPrelockRestarts)
	}
	if !rowExists(t, "issue", f.victimIssue) {
		t.Fatalf("the failed delete removed the victim issue")
	}
	for _, id := range append(s.lateIssues, f.victimIssue) {
		if !issueLockableNow(t, id) {
			t.Fatalf("issue %s is still locked after the delete gave up", id)
		}
	}
}

// A run created after the pre-lock read names a comment of the deleted issue.
func TestIssueDeleteLocksTheIssueOfARunThatNamesALateCommentReference(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	f := newWorkspaceDeletePathFixture(t, "prelock-issue-comment")
	var commentID string
	if err := testPool.QueryRow(context.Background(), `
INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content)
VALUES ($1, $2, 'member', $3, 'trigger') RETURNING id`, f.victimID, f.victimIssue, testUserID).Scan(&commentID); err != nil {
		t.Fatalf("create comment: %v", err)
	}
	late := lateRun{workspaceID: f.neighbourID, agentID: f.neighbourAgent, runtimeID: f.neighbourRuntime, comment: commentID}
	s := &prelockStarter{t: t, inner: testHandler.TxStarter, listMarker: "IssuesOfIssueDelete",
		probeMarker: "name: LockIssueForDelete", late: func() string { return late.create(t) }}

	withPrelockStarter(s, func() {
		if err := deleteVictimIssue(t, f.victimIssue); err != nil {
			t.Fatalf("delete issue: %v", err)
		}
	})
	requireLateIssueHeld(t, s)
}

// Once the pre-lock has finished, no new run can name a run or a comment the
// delete is about to remove: the insert waits for the delete instead of landing
// a reference the delete never saw.
func TestIssueDeleteFreezesTheRowsALateReferenceWouldPoint(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	f := newWorkspaceDeletePathFixture(t, "prelock-issue-freeze")
	parent := insertDeletePathTask(t, f.victimAgent, f.victimIssue, f.victimRuntime)
	var commentID string
	if err := testPool.QueryRow(context.Background(), `
INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content)
VALUES ($1, $2, 'member', $3, 'trigger') RETURNING id`, f.victimID, f.victimIssue, testUserID).Scan(&commentID); err != nil {
		t.Fatalf("create comment: %v", err)
	}
	var childErr, commentErr error
	s := &prelockStarter{t: t, inner: testHandler.TxStarter, listMarker: "IssuesOfIssueDelete",
		probeMarker: "name: LockIssueForDelete"}
	s.onProbe = func() {
		base := lateRun{workspaceID: f.neighbourID, agentID: f.neighbourAgent, runtimeID: f.neighbourRuntime}
		child, byComment := base, base
		child.parent, byComment.comment = parent, commentID
		childErr = child.insert(t, insertLateIssue(t, f.neighbourID), true)
		commentErr = byComment.insert(t, insertLateIssue(t, f.neighbourID), true)
	}

	withPrelockStarter(s, func() {
		if err := deleteVictimIssue(t, f.victimIssue); err != nil {
			t.Fatalf("delete issue: %v", err)
		}
	})
	requireLockTimeout(t, childErr, "a child of a run of the deleted issue")
	requireLockTimeout(t, commentErr, "a run that names a comment of the deleted issue")
}

// A run created after the pre-lock read names the comment being deleted.
func TestCommentDeleteLocksTheIssueOfARunThatNamesALateCommentReference(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	f := newWorkspaceDeletePathFixture(t, "prelock-comment")
	var commentID string
	if err := testPool.QueryRow(context.Background(), `
INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content)
VALUES ($1, $2, 'member', $3, 'trigger') RETURNING id`, f.victimID, f.victimIssue, testUserID).Scan(&commentID); err != nil {
		t.Fatalf("create comment: %v", err)
	}
	late := lateRun{workspaceID: f.neighbourID, agentID: f.neighbourAgent, runtimeID: f.neighbourRuntime, comment: commentID}
	var freezeErr error
	s := &prelockStarter{t: t, inner: testHandler.TxStarter, listMarker: "IssuesOfCommentDelete",
		probeMarker: "name: LockCommentForDelete", late: func() string { return late.create(t) }}
	s.onProbe = func() {
		freezeErr = late.insert(t, insertLateIssue(t, f.neighbourID), true)
	}

	withPrelockStarter(s, func() {
		if _, err := testHandler.deleteComment(context.Background(), parseUUID(commentID), parseUUID(f.victimID)); err != nil {
			t.Fatalf("delete comment: %v", err)
		}
	})
	requireLateIssueHeld(t, s)
	requireLockTimeout(t, freezeErr, "a run that names the comment being deleted")
}

// systemAgentSession is a chat session whose carrier agent is a system agent
// with a completed run, and a queued run of the session.
type systemAgentSession struct {
	sessionID, agentID, parent, queued string
}

func newSystemAgentSession(t *testing.T) systemAgentSession {
	t.Helper()
	ctx := context.Background()
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
	dbfx.Exec(t, `UPDATE agent SET kind = 'system', system_key = $2 WHERE id = $1`, agentID, "agent_builder:"+uuid.NewString())
	var sessionID, queued string
	if err := testPool.QueryRow(ctx, `
INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status)
VALUES ($1, $2, $3, 'prelock', 'active') RETURNING id`, testWorkspaceID, agentID, testUserID).Scan(&sessionID); err != nil {
		t.Fatalf("create chat session: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM chat_session WHERE id = $1`, sessionID) })
	if err := testPool.QueryRow(ctx, `
INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, priority)
VALUES ($1, $2, $3, 'queued', 0) RETURNING id`, agentID, runtimeID, sessionID).Scan(&queued); err != nil {
		t.Fatalf("create queued run: %v", err)
	}
	return systemAgentSession{
		sessionID: sessionID, agentID: agentID, queued: queued,
		parent: insertDeletePathTask(t, agentID, dbfx.Issue(t, "carrier run issue"), runtimeID),
	}
}

func deleteChatSession(t *testing.T, sessionID string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/chat/sessions/"+sessionID, nil)
	req.Header.Set("X-User-ID", testUserID)
	req = withURLParam(req, "sessionId", sessionID)
	req = withChatTestWorkspaceCtx(t, req)
	w := httptest.NewRecorder()
	testHandler.DeleteChatSession(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteChatSession = %d, want 204: %s", w.Code, w.Body.String())
	}
}

// A child run created after the pre-lock read names a third issue, and the
// pre-lock runs before the delete holds any run row of the session.
func TestSystemAgentDeleteLocksTheIssueOfAChildCreatedAfterThePrelockRead(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	f := newWorkspaceDeletePathFixture(t, "prelock-agent")
	sess := newSystemAgentSession(t)
	late := lateRun{workspaceID: f.neighbourID, agentID: f.neighbourAgent, runtimeID: f.neighbourRuntime, parent: sess.parent}
	var freezeErr error
	s := &prelockStarter{t: t, inner: testHandler.TxStarter, listMarker: "IssuesOfAgentBuilderTaskChildren",
		probeMarker: "name: DeleteSystemAgentByID", taskRow: sess.queued,
		late: func() string { return late.create(t) }}
	s.onProbe = func() {
		freezeErr = late.insert(t, insertLateIssue(t, f.neighbourID), true)
	}

	withPrelockStarter(s, func() { deleteChatSession(t, sess.sessionID) })
	requireLateIssueHeld(t, s)
	requireLockTimeout(t, freezeErr, "a child of a run of the agent being deleted")
	if s.taskHeldAtList {
		t.Fatalf("the pre-lock ran while the delete already held a run row of the session: " +
			"a restart of the pre-lock would take a new issue lock after task rows")
	}
}

// The same for the system agents a runtime teardown removes.
func TestRuntimeTeardownLocksTheIssueOfAChildCreatedAfterThePrelockRead(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	f := newWorkspaceDeletePathFixture(t, "prelock-runtime")
	runtimeID := createClaimReclaimRuntime(t, ctx, "prelock runtime "+uuid.NewString()[:8])
	carrier, parent := makeSystemAgentWithRun(t, runtimeID, dbfx.Issue(t, "carrier run issue"))
	var queued string
	if err := testPool.QueryRow(ctx, `
INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
VALUES ($1, $2, $3, 'queued', 0) RETURNING id`, carrier, runtimeID, dbfx.Issue(t, "queued run issue")).Scan(&queued); err != nil {
		t.Fatalf("create queued run: %v", err)
	}
	late := lateRun{workspaceID: f.neighbourID, agentID: f.neighbourAgent, runtimeID: f.neighbourRuntime, parent: parent}
	var freezeErr error
	s := &prelockStarter{t: t, inner: testHandler.TxStarter, listMarker: "IssuesOfSystemAgentTaskChildrenByRuntime",
		probeMarker: "name: DeleteSystemAgentsByRuntime", taskRow: queued,
		late: func() string { return late.create(t) }}
	s.onProbe = func() {
		freezeErr = late.insert(t, insertLateIssue(t, f.neighbourID), true)
	}

	withPrelockStarter(s, func() {
		tx, err := testHandler.TxStarter.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		if _, err := service.TeardownRuntime(ctx, testHandler.Queries.WithTx(tx), parseUUID(runtimeID),
			service.RuntimeTeardownOptions{CancelNonTerminalTasks: true}); err != nil {
			t.Fatalf("teardown runtime: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	})
	requireLateIssueHeld(t, s)
	requireLockTimeout(t, freezeErr, "a child of a run of a system agent of the runtime")
	if s.taskHeldAtList {
		t.Fatalf("the pre-lock ran while the teardown already held a run row of the runtime")
	}
}
