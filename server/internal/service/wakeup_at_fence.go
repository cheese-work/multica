package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-8962 (plan CHE-8945 r6, step 2): a one-shot `at --watch issue` fire is
// compared with the fence its creating run's claim stored (CHE-8961).

var (
	// atFenceMinBudget is the time the dispatch deadline must still leave for
	// the compare to run: 300 ms statement timeout, 50 ms savepoint rollback and
	// 800 ms reserved for delivery. A variable so tests can raise it.
	atFenceMinBudget = 1150 * time.Millisecond
	// atFenceRead runs the projection inside the compare savepoint. A variable so
	// tests can make it slow or count calls.
	atFenceRead = func(ctx context.Context, sp pgx.Tx, arg db.IssueWatchFenceParams) (db.IssueWatchFenceRow, error) {
		return db.New(sp).IssueWatchFence(ctx, arg)
	}
	// atFenceRollback ends the compare savepoint. A variable so tests can fail it.
	atFenceRollback = func(ctx context.Context, sp pgx.Tx) error { return sp.Rollback(ctx) }
)

// atFenceReasonNoDeadline is the Reason when the dispatch context has no
// deadline: the compare cannot prove its time budget, so the fire runs.
const atFenceReasonNoDeadline = "no_deadline"

const (
	// atFenceMaxAge is how long after R completed its fence still counts.
	atFenceMaxAge = 6 * time.Hour
	// atFenceCompareTimeout is the server-side statement timeout of the compare.
	// The budget gate keeps it below the context deadline: pgx closes the
	// connection when a context expires mid-query, which would end the whole
	// fire transaction instead of falling back to a normal fire.
	atFenceCompareTimeout = "300ms"
	// atFenceCommentCap is the most comment rows a fence may cover (as at claim).
	atFenceCommentCap = 200
)

// atFenceResult is what the compare found. Defer is true only when every
// condition of the fire rule holds; Reason names the first one that did not.
type atFenceResult struct {
	Defer    bool
	RunID    pgtype.UUID
	TimeLeft time.Duration
	Reason   string
}

// atFenceApplies reports whether the wakeup is a one-shot time wakeup that
// watches only the issue and has a creating run.
func atFenceApplies(w db.IssueWakeup) bool {
	return w.Kind == "at" && len(w.WatchInputs) == 1 && w.WatchInputs[0] == "issue" && w.SourceTaskID.Valid
}

// atFenceCompare reports whether the fire may be deferred. It never changes
// durable state: the compare runs in a savepoint that is always rolled back.
// A read error or a missing condition means "run" (Defer false, nil error); only
// a failed rollback returns an error, so the receipt stays pending for the next
// pass.
func atFenceCompare(ctx context.Context, tx pgx.Tx, w db.IssueWakeup) (atFenceResult, error) {
	if !atFenceApplies(w) {
		return atFenceResult{Reason: "not_applicable"}, nil
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return atFenceResult{Reason: atFenceReasonNoDeadline}, nil
	}
	left := time.Until(deadline)
	if left < atFenceMinBudget {
		return atFenceResult{Reason: "low_budget", TimeLeft: left}, nil
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return atFenceResult{Reason: "savepoint_failed", TimeLeft: left}, nil
	}
	res := atFenceResult{Reason: "read_error", TimeLeft: left, RunID: w.SourceTaskID}
	func() {
		if _, err := sp.Exec(ctx, "SELECT set_config('statement_timeout', $1, true)", atFenceCompareTimeout); err != nil {
			return
		}
		res = atFenceLoad(ctx, sp, w, res)
	}()
	if err := atFenceRollback(ctx, sp); err != nil {
		return atFenceResult{}, err
	}
	return res, nil
}

// atFenceLoad loads R and its fence and compares them with the issue now.
func atFenceLoad(ctx context.Context, sp pgx.Tx, w db.IssueWakeup, res atFenceResult) atFenceResult {
	var raw []byte
	err := sp.QueryRow(ctx, `SELECT issue_snapshot->'watch' FROM agent_task_queue
		WHERE id = $1 AND agent_id = $2 AND issue_id = $3 AND status = 'completed'
		  AND completed_at > now() - ($4::bigint * interval '1 second')`,
		w.SourceTaskID, w.AgentID, w.IssueID, int64(atFenceMaxAge/time.Second)).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		res.Reason = "no_creating_run"
		return res
	}
	if err != nil {
		return res
	}
	var fence struct {
		Version  int       `json:"v"`
		Row      string    `json:"row"`
		Revision int64     `json:"revision"`
		Comments string    `json:"comments"`
		At       time.Time `json:"at"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &fence) != nil || fence.Version != 2 || fence.At.IsZero() {
		res.Reason = "no_fence"
		return res
	}
	now, err := atFenceRead(ctx, sp, db.IssueWatchFenceParams{
		IssueID: w.IssueID, RunID: w.SourceTaskID, AgentID: w.AgentID,
		FenceAt: pgtype.Timestamptz{Time: fence.At, Valid: true},
	})
	if err != nil {
		return res
	}
	switch {
	case now.Attached:
		res.Reason = "attached"
	case now.CommentRows > atFenceCommentCap:
		res.Reason = "over_cap"
	case now.RowHash != fence.Row:
		res.Reason = "row_changed"
	case now.CommentHash != fence.Comments:
		res.Reason = "comments_changed"
	case now.Revision-fence.Revision != now.Credit:
		res.Reason = "revision_changed"
	default:
		res.Defer, res.Reason = true, "unchanged"
	}
	return res
}
