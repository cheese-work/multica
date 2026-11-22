package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/dispatch"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// GetIssueRunBudget returns the issue's run budget: runs used in the open
// window against the limit in force, its state, and the suggested raise. Any
// reader of the issue may see it; an issue with no budget says so.
func (h *Handler) GetIssueRunBudget(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	snap, err := service.LoadRunBudgetSnapshot(r.Context(), h.Queries, issue)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read the run budget")
		return
	}
	next, err := h.loadRunBudgetNextStep(r.Context(), h.Queries, issue, snap)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read the run budget")
		return
	}
	view := runBudgetView{RunBudgetSnapshot: snap, OpenCard: next.OpenCard, NextCommand: next.NextCommand}
	if err := h.addWeeklyRunGuard(r.Context(), issue, &view); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read the run budget")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// GetIssueStageQueue returns where the issue's stage run stands in its owner's
// WIP queue (CHE-3611): admitted, or queued at a position behind the PRs ahead.
func (h *Handler) GetIssueStageQueue(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	snap, err := service.LoadStageQueueSnapshot(r.Context(), h.Queries, issue)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read the stage queue")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// RunBudgetDirectRouteGone answers the retired direct raise and top-up routes
// (CHE-2798 S1c). Both opened spend from one POST; a raise or top-up now goes
// through a decision action request that an owner approves (A2b meter, CLI).
// No credential reaches either write, an interactive session included.
func (h *Handler) RunBudgetDirectRouteGone(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusGone, "this route is removed; request a run budget raise or top-up and approve it in the web app (POST /api/issues/{id}/run-budget/requests)")
}

var errRunBudgetNotBudgeted = errors.New("the issue has no workflow run budget to top up")

// topUpRunBudget opens a new window for the issue, as a member's top-up or the
// auto-approver's. guard, when set, runs under the boundary lock once the window
// is written and before anything is forgiven; an error from it aborts the
// top-up. after, when set, runs in the same transaction once the new window is
// open, so work that needs the window (the refused run) commits with it, and
// the function it returns runs after the commit. It returns the run cap the new
// window grants.
func (h *Handler) topUpRunBudget(ctx context.Context, issue db.Issue, actorType string, actorID pgtype.UUID, details []byte, guard func(context.Context, *db.Queries) error, after func(context.Context, *db.Queries) (func(), error)) (int, error) {
	// Moves and top-ups on one issue start their windows one at a time, and
	// wait for their turn before they hold a connection.
	leave, err := h.enterRunBudgetBoundary(ctx, issue.ID)
	if err != nil {
		return 0, err
	}
	defer leave()
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)
	if err := qtx.LockIssueRunBudgetBoundary(ctx, issue.ID); err != nil {
		return 0, err
	}
	// The activity row below starts the next window. Writing the current one
	// first keeps its credit or debt in the ledger; the lock it takes holds to
	// the commit.
	max, err := service.OpenRunBudgetWindow(ctx, qtx, issue.ID)
	if err != nil {
		return 0, err
	}
	if max == 0 {
		return 0, errRunBudgetNotBudgeted
	}
	if guard != nil {
		if err := guard(ctx, qtx); err != nil {
			return 0, err
		}
	}
	if err := service.ForgiveRunBudgetDebt(ctx, qtx, issue.ID); err != nil {
		return 0, err
	}
	// The transaction began before the wait for the lock, so now() would sort
	// this boundary before any move recorded during the wait.
	startedAt, err := qtx.RunBudgetBoundaryClock(ctx)
	if err != nil {
		return 0, err
	}
	if _, err := qtx.CreateActivity(ctx, db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: actorType, Valid: true},
		ActorID: actorID, Action: "run_budget_top_up", Details: details,
		CreatedAt: startedAt,
	}); err != nil {
		return 0, fmt.Errorf("record top-up: %w", err)
	}
	if _, err := service.OpenRunBudgetWindow(ctx, qtx, issue.ID); err != nil {
		return 0, err
	}
	var afterCommit func()
	if after != nil {
		if afterCommit, err = after(ctx, qtx); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	if afterCommit != nil {
		afterCommit()
	}
	return max, nil
}

// openRunBudgetWindow brings the issue's ledger to the window in force, in its
// own transaction.
func (h *Handler) openRunBudgetWindow(ctx context.Context, issue db.Issue) error {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := service.OpenRunBudgetWindow(ctx, h.Queries.WithTx(tx), issue.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// boundaryQueue orders the requests of this process that want one issue's
// boundary lock. The database lock lives in a transaction, which holds a pooled
// connection while it waits: if every waiter held one, the request that holds
// the lock could not get the connections it needs to finish. Only the head of
// the queue waits in the database.
var boundaryQueue = struct {
	sync.Mutex
	turns map[pgtype.UUID]*boundaryTurn
}{turns: map[pgtype.UUID]*boundaryTurn{}}

type boundaryTurn struct {
	held    chan struct{} // holds one token while a request has the turn
	waiting int
}

// boundarySlots bounds, per pool, how many requests hold a boundary lock at
// once. Each holder keeps one connection and needs more to finish, so holders
// of different issues could otherwise take the whole pool between them. A
// quarter of the pool leaves each holder room for the connections of its
// write, its publish and the lock of the issue a listener moves next.
var boundarySlots = struct {
	sync.Mutex
	byPool map[txStarter]chan struct{}
}{byPool: map[txStarter]chan struct{}{}}

func boundarySlotsOf(starter txStarter) chan struct{} {
	boundarySlots.Lock()
	defer boundarySlots.Unlock()
	slots, ok := boundarySlots.byPool[starter]
	if !ok {
		poolSize := int32(4)
		if pool, isPool := starter.(interface{ Stat() *pgxpool.Stat }); isPool {
			poolSize = pool.Stat().MaxConns()
		}
		slots = make(chan struct{}, max(1, poolSize/4))
		boundarySlots.byPool[starter] = slots
	}
	return slots
}

// enterRunBudgetBoundary waits, holding no connection, for the issue's turn
// and then for a slot, and returns the function that gives both back. No
// caller takes a second boundary lock while it holds one, so a slot never
// waits on its own holder.
func (h *Handler) enterRunBudgetBoundary(ctx context.Context, issueID pgtype.UUID) (leave func(), err error) {
	return h.enterRunBudgetBoundaries(ctx, issueID)
}

// enterRunBudgetBoundaries is one boundary entry for several issues: it takes
// every issue's turn, in id order so two batches never wait on each other, then
// one slot. A batch of top-ups holds one slot, not one per issue.
func (h *Handler) enterRunBudgetBoundaries(ctx context.Context, issueIDs ...pgtype.UUID) (leave func(), err error) {
	issueIDs = slices.Clone(issueIDs)
	slices.SortFunc(issueIDs, func(a, b pgtype.UUID) int { return bytes.Compare(a.Bytes[:], b.Bytes[:]) })
	issueIDs = slices.Compact(issueIDs)

	boundaryQueue.Lock()
	turns := make([]*boundaryTurn, len(issueIDs))
	for index, issueID := range issueIDs {
		turn := boundaryQueue.turns[issueID]
		if turn == nil {
			turn = &boundaryTurn{held: make(chan struct{}, 1)}
			boundaryQueue.turns[issueID] = turn
		}
		turn.waiting++
		turns[index] = turn
	}
	boundaryQueue.Unlock()

	slots := boundarySlotsOf(h.TxStarter)
	held := 0 // turns taken, in order
	var gotSlot bool
	leave = sync.OnceFunc(func() {
		if gotSlot {
			<-slots
		}
		for _, turn := range turns[:held] {
			<-turn.held
		}
		boundaryQueue.Lock()
		for index, turn := range turns {
			if turn.waiting--; turn.waiting == 0 {
				delete(boundaryQueue.turns, issueIDs[index])
			}
		}
		boundaryQueue.Unlock()
	})
	for _, turn := range turns {
		select {
		case turn.held <- struct{}{}:
			held++
		case <-ctx.Done():
			leave()
			return nil, ctx.Err()
		}
	}
	select {
	case slots <- struct{}{}:
		gotSlot = true
	case <-ctx.Done():
		leave()
		return nil, ctx.Err()
	}
	return leave, nil
}

var (
	// runBudgetBoundaryIdleTimeout ends a boundary transaction that has sat
	// idle this long. It is the server-side backstop for a holder whose
	// process stopped keeping it alive.
	runBudgetBoundaryIdleTimeout = 60 * time.Second
	// runBudgetBoundaryKeepalive is how often a live holder touches its
	// transaction. The protected status write runs on other connections, so
	// the boundary transaction is idle while the work is live; without this
	// the idle timeout would drop the lock under a healthy holder.
	runBudgetBoundaryKeepalive = 20 * time.Second
	// runBudgetBoundaryHoldLimit force-releases a boundary lock held this long,
	// freeing the in-process turn and pool slot the server cannot. The
	// keepalive stops here, so a leak ends within this limit.
	runBudgetBoundaryHoldLimit = 90 * time.Second
)

const runBudgetBoundaryRollbackTimeout = 5 * time.Second

// lockRunBudgetBoundary holds the issue's boundary lock until release runs;
// release is safe to call more than once. An agent's or the platform's status
// write takes it around the write and its grant. A person's move reads the
// status under the lock to decide whether it ends a window, so a write that
// could land between that read and the move's own write has to wait.
func (h *Handler) lockRunBudgetBoundary(ctx context.Context, issueID pgtype.UUID) (release func(), err error) {
	leave, err := h.enterRunBudgetBoundary(ctx, issueID)
	if err != nil {
		return nil, err
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		leave()
		return nil, err
	}
	// txMu serialises the keepalive with the rollback: a pgx connection runs
	// one statement at a time.
	var txMu sync.Mutex
	stop := make(chan struct{})
	release = sync.OnceFunc(func() {
		close(stop)
		txMu.Lock()
		defer txMu.Unlock()
		// The request may be cancelled by now; a rollback on a cancelled
		// context is never sent and would leave the lock to the server.
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runBudgetBoundaryRollbackTimeout)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
		leave()
	})
	// Backstop for a holder that stops keeping its transaction alive: the
	// server ends the transaction, and with it the lock, once it sits idle.
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL idle_in_transaction_session_timeout = %d", runBudgetBoundaryIdleTimeout.Milliseconds())); err != nil {
		release()
		return nil, err
	}
	if err := h.Queries.WithTx(tx).LockIssueRunBudgetBoundary(ctx, issueID); err != nil {
		release()
		return nil, err
	}
	go keepRunBudgetBoundary(ctx, issueID, tx, &txMu, stop, release, runBudgetBoundaryKeepalive, runBudgetBoundaryHoldLimit)
	return release, nil
}

// keepRunBudgetBoundary keeps a held boundary transaction from idling out
// while its holder works, and force-releases it at the hold limit. Reaching
// the limit means the holder leaked, or ran longer than any status write
// should; either way the in-process turn and slot are freed and the issue is
// logged.
func keepRunBudgetBoundary(ctx context.Context, issueID pgtype.UUID, tx pgx.Tx, txMu *sync.Mutex, stop <-chan struct{}, release func(), keepalive, holdLimit time.Duration) {
	started := time.Now()
	ticker := time.NewTicker(keepalive)
	defer ticker.Stop()
	limit := time.NewTimer(holdLimit)
	defer limit.Stop()
	for {
		select {
		case <-stop:
			return
		case <-limit.C:
			slog.Error("run budget: boundary lock held too long; releasing it", "issue_id", uuidToString(issueID), "held", time.Since(started).String())
			release()
			return
		case <-ticker.C:
			txMu.Lock()
			select {
			case <-stop:
				txMu.Unlock()
				return
			default:
			}
			pingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runBudgetBoundaryRollbackTimeout)
			_, err := tx.Exec(pingCtx, "SELECT 1")
			cancel()
			txMu.Unlock()
			if err != nil {
				slog.Error("run budget: boundary lock lost; releasing it", "issue_id", uuidToString(issueID), "held", time.Since(started).String(), "error", err)
				release()
				return
			}
		}
	}
}

// lockRunBudgetMove readies a person's status write, which starts the issue's
// next run-budget window when it changes the status: it takes the boundary
// lock, then writes the window the move is about to end. Whether the write
// changes the status is read under the lock, because a move that held the lock
// before may have changed it since the caller read the issue. The caller
// releases the lock after the move is published and its new window opened, so
// two moves never interleave their boundaries and the second one finds the
// first one's window written. An error means nothing was written and the
// caller must not write the move.
func (h *Handler) lockRunBudgetMove(ctx context.Context, issue db.Issue, requestedStatus string) (release func(), err error) {
	release, err = h.lockRunBudgetBoundary(ctx, issue.ID)
	if err != nil {
		return nil, err
	}
	current, err := h.Queries.GetIssue(ctx, issue.ID)
	if err != nil {
		release()
		return nil, err
	}
	if current.Status == requestedStatus {
		return release, nil
	}
	if err := h.openRunBudgetWindow(ctx, issue); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (h *Handler) beginRunBudgetMove(w http.ResponseWriter, r *http.Request, actorType string, issue db.Issue, requestedStatus string) (release func(), ok bool) {
	var err error
	if actorType == "member" {
		release, err = h.lockRunBudgetMove(r.Context(), issue, requestedStatus)
	} else {
		release, err = h.lockRunBudgetBoundary(r.Context(), issue.ID)
	}
	if err != nil {
		slog.Warn("run budget: prepare status move", "issue_id", uuidToString(issue.ID), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update issue")
		return nil, false
	}
	return release, true
}

// openRunBudgetWindowForMove opens the run-budget window a person's status move
// started, when the move is recorded rather than at the next admission. It runs
// after the event is published: the activity listener has written the move by
// then, and the window begins at that row. A failure is logged; the next
// admission opens the same window.
func (h *Handler) openRunBudgetWindowForMove(ctx context.Context, issue db.Issue) {
	if err := h.openRunBudgetWindow(ctx, issue); err != nil {
		slog.Warn("run budget: open window for status move", "issue_id", uuidToString(issue.ID), "error", err)
	}
}

// inRunBudgetTx runs a ledger write in its own transaction. A failure is
// logged and the caller's work stands: only the credit is missed, and the next
// event writes the same keys.
func (h *Handler) inRunBudgetTx(ctx context.Context, issueID pgtype.UUID, what string, fn func(*db.Queries) error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err == nil {
		defer tx.Rollback(ctx)
		if err = fn(h.Queries.WithTx(tx)); err == nil {
			err = tx.Commit(ctx)
		}
	}
	if err != nil {
		slog.Warn("run budget: "+what, "issue_id", uuidToString(issueID), "error", err)
	}
}

const errRecoveryCaused = "a budget recovery run cannot start work, create children or wakeups, or top up"

// grantRunBudgetStep grants the first entry of a status in the budget window
// when an agent or a workflow edge moved the issue. It runs after the event is
// published, like openRunBudgetWindowForMove: the activity listener has written
// the move, and the window's first status is read from it.
func (h *Handler) grantRunBudgetStep(ctx context.Context, issue db.Issue, prevStatus string) {
	h.inRunBudgetTx(ctx, issue.ID, "grant step", func(q *db.Queries) error {
		return service.GrantRunBudgetStep(ctx, q, issue, prevStatus, dispatch.BudgetRecoveryCaller(ctx))
	})
}

func (h *Handler) grantRunBudgetHead(ctx context.Context, issueID, prID pgtype.UUID, sha string) {
	h.inRunBudgetTx(ctx, issueID, "grant head", func(q *db.Queries) error {
		return service.GrantRunBudgetHead(ctx, q, issueID, prID, sha)
	})
}

func (h *Handler) refundSupersededRun(ctx context.Context, task db.AgentTaskQueue) {
	h.inRunBudgetTx(ctx, task.IssueID, "refund superseded run", func(q *db.Queries) error {
		return service.RefundSupersededRun(ctx, q, task)
	})
}
