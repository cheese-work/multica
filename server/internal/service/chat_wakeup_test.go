package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type chatWakeupFixture struct {
	pool      *pgxpool.Pool
	q         *db.Queries
	svc       *ChatWakeupService
	sessionID pgtype.UUID
	taskID    pgtype.UUID // running chat task inside the DM
	agentID   string
	runtimeID string
	userID    string
	issueID   string
}

func newChatWakeupFixture(t *testing.T) chatWakeupFixture {
	t.Helper()
	pool := sharedTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	_, userID, agentID, issueID := seedAttributionFixture(t, pool)
	workspaceID := ""
	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT workspace_id, runtime_id FROM agent WHERE id = $1`, agentID).Scan(&workspaceID, &runtimeID); err != nil {
		t.Fatalf("load agent: %v", err)
	}
	var sessionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id)
		VALUES ($1, $2, $3) RETURNING id`, workspaceID, agentID, userID).Scan(&sessionID); err != nil {
		t.Fatalf("seed chat session: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM chat_wakeup WHERE chat_session_id = $1`, sessionID)
		pool.Exec(bg, `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
		pool.Exec(bg, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		pool.Exec(bg, `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
		pool.Exec(bg, `DELETE FROM chat_session WHERE id = $1`, sessionID)
	})
	f := chatWakeupFixture{
		pool: pool, q: q, agentID: agentID, runtimeID: runtimeID, userID: userID, issueID: issueID,
		sessionID: util.MustParseUUID(sessionID),
		svc:       &ChatWakeupService{Tasks: &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}},
	}
	f.taskID = f.runningTask(t, f.sessionID)
	return f
}

// runningTask seeds the chat task whose run asks for the wakeup.
func (f chatWakeupFixture) runningTask(t *testing.T, sessionID pgtype.UUID) pgtype.UUID {
	t.Helper()
	task, err := f.q.CreateChatTask(context.Background(), db.CreateChatTaskParams{
		ID: util.MustParseUUID(newTestUUID(t, f.pool)), AgentID: util.MustParseUUID(f.agentID),
		RuntimeID: util.MustParseUUID(f.runtimeID), Priority: 2, ChatSessionID: sessionID,
		InitiatorUserID: util.MustParseUUID(f.userID),
	})
	if err != nil {
		t.Fatalf("seed chat task: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE agent_task_queue SET status = 'running' WHERE id = $1`, task.ID); err != nil {
		t.Fatalf("run chat task: %v", err)
	}
	return task.ID
}

func newTestUUID(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f chatWakeupFixture) create(t *testing.T, after time.Duration, instruction string) db.ChatWakeup {
	t.Helper()
	w, err := f.svc.Create(context.Background(), ChatWakeupInput{TaskID: f.taskID, At: time.Now().Add(after), Instruction: instruction})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return w
}

func (f chatWakeupFixture) makeDue(t *testing.T, w db.ChatWakeup) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE chat_wakeup SET fire_at = now() - interval '1 second' WHERE id = $1`, w.ID); err != nil {
		t.Fatal(err)
	}
}

func (f chatWakeupFixture) status(t *testing.T, w db.ChatWakeup) string {
	t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM chat_wakeup WHERE id = $1`, w.ID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (f chatWakeupFixture) queuedTurns(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1 AND status = 'queued'`, f.sessionID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A due wakeup sends its instruction into the DM as a labelled scheduled turn,
// with no human message, exactly once.
func TestChatWakeupTickSendsLabelledTurnOnce(t *testing.T) {
	f := newChatWakeupFixture(t)
	ctx := context.Background()
	w := f.create(t, time.Minute, "refresh Luna readiness")
	f.makeDue(t, w)

	for range 2 {
		if err := f.svc.Tick(ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}

	if got := f.status(t, w); got != "fired" {
		t.Fatalf("status = %q, want fired", got)
	}
	if n := f.queuedTurns(t); n != 1 {
		t.Fatalf("queued chat turns = %d, want 1", n)
	}
	var content string
	if err := f.pool.QueryRow(ctx, `
		SELECT m.content FROM chat_message m JOIN agent_task_queue t ON t.id = m.task_id
		WHERE m.chat_session_id = $1 AND t.status = 'queued'`, f.sessionID).Scan(&content); err != nil {
		t.Fatalf("load scheduled message: %v", err)
	}
	if !strings.Contains(content, "refresh Luna readiness") || !strings.Contains(strings.ToLower(content), "scheduled") {
		t.Fatalf("message = %q, want a scheduled label plus the instruction", content)
	}
}

func TestChatWakeupNotDueDoesNotFire(t *testing.T) {
	f := newChatWakeupFixture(t)
	w := f.create(t, time.Hour, "later")
	if err := f.svc.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := f.status(t, w); got != "pending" || f.queuedTurns(t) != 0 {
		t.Fatalf("status = %q, queued = %d; want pending, 0", got, f.queuedTurns(t))
	}
}

func TestChatWakeupCancelStopsFire(t *testing.T) {
	f := newChatWakeupFixture(t)
	w := f.create(t, time.Minute, "never")
	if err := f.svc.Cancel(context.Background(), f.taskID, w.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	f.makeDue(t, w)
	if err := f.svc.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := f.status(t, w); got != "cancelled" || f.queuedTurns(t) != 0 {
		t.Fatalf("status = %q, queued = %d; want cancelled, 0", got, f.queuedTurns(t))
	}
}

func TestChatWakeupArchivedDMStopsFire(t *testing.T) {
	f := newChatWakeupFixture(t)
	w := f.create(t, time.Minute, "never")
	f.makeDue(t, w)
	if _, err := f.pool.Exec(context.Background(), `UPDATE chat_session SET status = 'archived' WHERE id = $1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := f.status(t, w); got != "cancelled" || f.queuedTurns(t) != 0 {
		t.Fatalf("status = %q, queued = %d; want cancelled, 0", got, f.queuedTurns(t))
	}
}

func TestChatWakeupRejectsFourthPendingAndFarFuture(t *testing.T) {
	f := newChatWakeupFixture(t)
	for range MaxPendingChatWakeups {
		f.create(t, time.Minute, "ok")
	}
	_, err := f.svc.Create(context.Background(), ChatWakeupInput{TaskID: f.taskID, At: time.Now().Add(time.Minute), Instruction: "fourth"})
	if !errors.Is(err, ErrChatWakeupLimit) {
		t.Fatalf("fourth Create error = %v, want ErrChatWakeupLimit", err)
	}

	g := newChatWakeupFixture(t)
	_, err = g.svc.Create(context.Background(), ChatWakeupInput{TaskID: g.taskID, At: time.Now().Add(25 * time.Hour), Instruction: "far"})
	if !errors.Is(err, ErrChatWakeupTooFar) {
		t.Fatalf("25h Create error = %v, want ErrChatWakeupTooFar", err)
	}
	_, err = g.svc.Create(context.Background(), ChatWakeupInput{TaskID: g.taskID, At: time.Now().Add(-time.Minute), Instruction: "past"})
	if !errors.Is(err, ErrChatWakeupPast) {
		t.Fatalf("past Create error = %v, want ErrChatWakeupPast", err)
	}
}

// Only a running task of this very DM may manage its wakeups: not another
// DM's task, not an issue run, not a finished task.
func TestChatWakeupOnlyOwnDMTaskMayManage(t *testing.T) {
	f := newChatWakeupFixture(t)
	other := newChatWakeupFixture(t)
	ctx := context.Background()
	w := f.create(t, time.Minute, "mine")

	// Another DM's task cannot create into, list, or cancel this DM's wakeups:
	// a task identifies its own DM, so these act on the other DM, never this one.
	if got, err := other.svc.List(ctx, other.taskID); err != nil || len(got) != 0 {
		t.Fatalf("other DM list = %v, %v; want empty", got, err)
	}
	if err := other.svc.Cancel(ctx, other.taskID, w.ID); !errors.Is(err, ErrChatWakeupNotFound) {
		t.Fatalf("cross-DM Cancel error = %v, want ErrChatWakeupNotFound", err)
	}
	if got := f.status(t, w); got != "pending" {
		t.Fatalf("status after cross-DM cancel = %q, want pending", got)
	}

	// A task that is not a running chat task is refused.
	var issueTaskID pgtype.UUID
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
		VALUES ($1, $2, $3, 'running', 2) RETURNING id`, f.agentID, f.runtimeID, f.issueID).Scan(&issueTaskID); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.Create(ctx, ChatWakeupInput{TaskID: issueTaskID, At: time.Now().Add(time.Minute), Instruction: "x"})
	if !errors.Is(err, ErrChatWakeupNotOwner) {
		t.Fatalf("issue-run Create error = %v, want ErrChatWakeupNotOwner", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, f.taskID); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Create(ctx, ChatWakeupInput{TaskID: f.taskID, At: time.Now().Add(time.Minute), Instruction: "x"})
	if !errors.Is(err, ErrChatWakeupNotOwner) {
		t.Fatalf("finished-task Create error = %v, want ErrChatWakeupNotOwner", err)
	}
}

// An archive that commits after Create read the session as active, but before
// Create took the session lock, must still refuse the wakeup.
func TestChatWakeupCreateRefusedWhenArchiveCommitsWhileWaitingForLock(t *testing.T) {
	f := newChatWakeupFixture(t)
	ctx := context.Background()
	archiver, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin archive tx: %v", err)
	}
	defer archiver.Rollback(ctx)
	if _, err := archiver.Exec(ctx, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, f.sessionID); err != nil {
		t.Fatalf("archive session: %v", err)
	}
	created := make(chan error, 1)
	go func() {
		_, err := f.svc.Create(ctx, ChatWakeupInput{TaskID: f.taskID, At: time.Now().Add(time.Hour), Instruction: "refresh"})
		created <- err
	}()
	// Create has read the session as active and is now blocked on the row lock.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query ILIKE '%chat_session%'`).Scan(&waiting); err != nil {
			t.Fatalf("poll lock waiters: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Create never blocked on the session lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := archiver.Commit(ctx); err != nil {
		t.Fatalf("commit archive: %v", err)
	}
	select {
	case err := <-created:
		if !errors.Is(err, ErrChatWakeupNotOwner) {
			t.Fatalf("Create after archive = %v, want ErrChatWakeupNotOwner", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Create did not return after the archive committed")
	}
	var pending int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM chat_wakeup WHERE chat_session_id = $1 AND status = 'pending'`, f.sessionID).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending wakeups on archived DM = %d, %v; want 0", pending, err)
	}
}
